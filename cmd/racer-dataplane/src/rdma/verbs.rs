//! The only Rust unsafe boundary. Provider layouts stay in native/rdma.c.
//! Handles are worker-local; failed teardown deliberately retains DMA ownership.
use crate::error::{Error, Result};
use std::{
    any::Any,
    cell::{Cell, RefCell},
    collections::BTreeMap,
    ffi::{CStr, c_char, c_int, c_void},
    ptr::NonNull,
    rc::Rc,
    task::{Context, Poll, Waker},
};

pub struct Verbs;

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

#[repr(C)]
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Endpoint {
    pub gid: [u8; 16],
    pub qpn: u32,
    pub psn: u32,
    pub mtu: u32,
    pub lid: u16,
    pub port: u8,
    pub link_layer: u8,
}

impl Endpoint {
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

#[repr(C)]
#[derive(Default, Clone, Copy)]
struct Completion {
    id: u64,
    status: u32,
    opcode: u32,
}

// This ABI consists only of fixed-width scalars, opaque pointers and repr(C)
// records. Every symbol is version-checked before any native handle is created.
struct Api {
    library: Option<NonNull<c_void>>,
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
    bind: unsafe extern "C" fn(*mut c_void, *mut c_void, *mut c_void, u32, u64) -> c_int,
    invalidate: unsafe extern "C" fn(*mut c_void, u32, u64) -> c_int,
    write: unsafe extern "C" fn(*mut c_void, *mut c_void, u64, u32, u64) -> c_int,
    poll: unsafe extern "C" fn(*mut c_void, *mut Completion, u32) -> c_int,
}

impl Api {
    #[cfg(not(all(feature = "rdma", target_os = "linux")))]
    fn load() -> Result<Rc<Self>> {
        Err(Error::Unavailable)
    }

    #[cfg(all(feature = "rdma", target_os = "linux"))]
    fn load() -> Result<Rc<Self>> {
        // The loader search path is administrator-controlled. No peer-supplied
        // library names or paths are accepted.
        unsafe {
            let library = NonNull::new(libc::dlopen(
                c"libracer_rdma.so.1".as_ptr(),
                libc::RTLD_NOW | libc::RTLD_LOCAL,
            ))
            .ok_or(Error::Unavailable)?;
            struct Guard(NonNull<c_void>);
            impl Drop for Guard {
                fn drop(&mut self) {
                    unsafe {
                        libc::dlclose(self.0.as_ptr());
                    }
                }
            }
            let guard = Guard(library);
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
            let version = sym!("racer_rdma_abi", unsafe extern "C" fn() -> u32);
            if version() != 1 {
                return Err(Error::Unavailable);
            }
            let api = Self {
                library: Some(library),
                discover: sym!(
                    "racer_rdma_discover",
                    unsafe extern "C" fn(*mut Port, u32) -> c_int
                ),
                open: sym!(
                    "racer_rdma_open",
                    unsafe extern "C" fn(*const c_char) -> *mut c_void
                ),
                close: sym!(
                    "racer_rdma_close",
                    unsafe extern "C" fn(*mut c_void) -> c_int
                ),
                qp: sym!(
                    "racer_rdma_qp",
                    unsafe extern "C" fn(*mut c_void, u8, u32, *mut u32) -> *mut c_void
                ),
                connect: sym!(
                    "racer_rdma_connect",
                    unsafe extern "C" fn(*mut c_void, *const Endpoint, *const Endpoint) -> c_int
                ),
                stop: sym!(
                    "racer_rdma_stop",
                    unsafe extern "C" fn(*mut c_void) -> c_int
                ),
                qp_free: sym!(
                    "racer_rdma_qp_free",
                    unsafe extern "C" fn(*mut c_void) -> c_int
                ),
                register: sym!(
                    "racer_rdma_register",
                    unsafe extern "C" fn(*mut c_void, u32) -> *mut c_void
                ),
                deregister: sym!(
                    "racer_rdma_deregister",
                    unsafe extern "C" fn(*mut c_void) -> c_int
                ),
                bytes: sym!(
                    "racer_rdma_bytes",
                    unsafe extern "C" fn(*mut c_void) -> *mut u8
                ),
                window: sym!(
                    "racer_rdma_window",
                    unsafe extern "C" fn(*mut c_void, *mut u32) -> *mut c_void
                ),
                window_free: sym!(
                    "racer_rdma_window_free",
                    unsafe extern "C" fn(*mut c_void) -> c_int
                ),
                bind: sym!(
                    "racer_rdma_bind",
                    unsafe extern "C" fn(*mut c_void, *mut c_void, *mut c_void, u32, u64) -> c_int
                ),
                invalidate: sym!(
                    "racer_rdma_invalidate",
                    unsafe extern "C" fn(*mut c_void, u32, u64) -> c_int
                ),
                write: sym!(
                    "racer_rdma_write",
                    unsafe extern "C" fn(*mut c_void, *mut c_void, u64, u32, u64) -> c_int
                ),
                poll: sym!(
                    "racer_rdma_poll",
                    unsafe extern "C" fn(*mut c_void, *mut Completion, u32) -> c_int
                ),
            };
            std::mem::forget(guard);
            Ok(Rc::new(api))
        }
    }
}
impl Drop for Api {
    fn drop(&mut self) {
        if let Some(library) = self.library {
            unsafe {
                libc::dlclose(library.as_ptr());
            }
        }
    }
}

pub struct DeviceHandle {
    api: Rc<Api>,
    raw: NonNull<c_void>,
    pub name: String,
    pub endpoint: Endpoint,
}
impl Drop for DeviceHandle {
    fn drop(&mut self) {
        if unsafe { (self.api.close)(self.raw.as_ptr()) } != 0 {
            std::mem::forget(self.api.clone());
        }
    }
}

impl Verbs {
    pub fn discover(&self) -> Result<Vec<DeviceHandle>> {
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
            devices.push(DeviceHandle {
                api: api.clone(),
                raw,
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
}

/// Native memory is never referenced while remotely writable or locally in flight.
/// Quota is attached inside the native owner, including intentional quarantine leaks.
pub(crate) struct Region {
    device: Rc<DeviceHandle>,
    raw: NonNull<c_void>,
    length: usize,
    quota: Option<Box<dyn Any>>,
    busy: Cell<bool>,
}
impl Region {
    pub(crate) fn new(
        device: Rc<DeviceHandle>,
        length: usize,
        quota: Box<dyn Any>,
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
            quota: Some(quota),
            busy: Cell::new(false),
        }))
    }
    pub(crate) fn address(&self) -> u64 {
        unsafe { (self.device.api.bytes)(self.raw.as_ptr()) as u64 }
    }
    pub(crate) fn length(&self) -> usize {
        self.length
    }
    pub(crate) fn copy_from(&self, bytes: &[u8]) -> Result<()> {
        if self.busy.get() || bytes.len() != self.length {
            return Err(Error::InvalidRequest);
        }
        unsafe {
            std::ptr::copy_nonoverlapping(
                bytes.as_ptr(),
                (self.device.api.bytes)(self.raw.as_ptr()),
                self.length,
            );
        }
        Ok(())
    }
    pub(crate) fn copy_to(&self) -> Result<Vec<u8>> {
        if self.busy.get() {
            return Err(Error::Unavailable);
        }
        Ok(unsafe {
            std::slice::from_raw_parts((self.device.api.bytes)(self.raw.as_ptr()), self.length)
        }
        .to_vec())
    }
}
impl Drop for Region {
    fn drop(&mut self) {
        if unsafe { (self.device.api.deregister)(self.raw.as_ptr()) } != 0 {
            std::mem::forget(self.device.clone());
            std::mem::forget(self.quota.take());
        }
    }
}

pub(crate) struct Window {
    device: Rc<DeviceHandle>,
    raw: NonNull<c_void>,
    pub(crate) key: u32,
    region: Rc<Region>,
}
impl Drop for Window {
    fn drop(&mut self) {
        if unsafe { (self.device.api.window_free)(self.raw.as_ptr()) } != 0 {
            std::mem::forget(self.device.clone());
            std::mem::forget(self.region.clone());
        }
    }
}

#[derive(Default)]
struct TicketState {
    result: Cell<Option<Result<()>>>,
    waker: RefCell<Option<Waker>>,
}
#[derive(Clone)]
pub(crate) struct Ticket(Rc<TicketState>);
impl Ticket {
    pub(crate) fn poll(&self, cx: &mut Context<'_>) -> Poll<Result<()>> {
        if let Some(result) = self.0.result.get() {
            Poll::Ready(result)
        } else {
            *self.0.waker.borrow_mut() = Some(cx.waker().clone());
            Poll::Pending
        }
    }
    pub(crate) fn result(&self) -> Option<Result<()>> {
        self.0.result.get()
    }
}

struct Pending {
    ticket: Ticket,
    opcode: u32,
    // Ownership survives future cancellation and failed CQ polling.
    region: Option<Rc<Region>>,
    _window: Option<Rc<Window>>,
}
impl Pending {
    fn finish(self, result: Result<()>) {
        std::sync::atomic::fence(std::sync::atomic::Ordering::Acquire);
        if let Some(region) = &self.region {
            region.busy.set(false);
        }
        self.ticket.0.result.set(Some(result));
        if let Some(waker) = self.ticket.0.waker.borrow_mut().take() {
            waker.wake();
        }
    }
}

pub struct QueuePairHandle {
    device: Rc<DeviceHandle>,
    raw: NonNull<c_void>,
    pub endpoint: Endpoint,
    stopped: Cell<bool>,
    terminating: Cell<bool>,
    connected: Cell<bool>,
    next: Cell<u64>,
    pending: RefCell<BTreeMap<u64, Pending>>,
    // Active remote grants persist independently of their bind CQE.
    windows: RefCell<Vec<Rc<Window>>>,
    expires: Cell<Option<std::time::Instant>>,
}
impl QueuePairHandle {
    pub(crate) fn new(device: Rc<DeviceHandle>) -> Result<Rc<Self>> {
        let mut qpn = 0;
        let raw = NonNull::new(unsafe {
            (device.api.qp)(device.raw.as_ptr(), device.endpoint.port, 64, &mut qpn)
        });
        let Some(raw) = raw else {
            if qpn == u32::MAX {
                std::mem::forget(device);
            }
            return Err(Error::Unavailable);
        };
        let mut psn = [0; 4];
        if getrandom::getrandom(&mut psn).is_err() {
            if unsafe { (device.api.qp_free)(raw.as_ptr()) } != 0 {
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
            device,
            raw,
            endpoint,
            stopped: Cell::new(false),
            terminating: Cell::new(false),
            connected: Cell::new(false),
            next: Cell::new(1),
            pending: RefCell::new(BTreeMap::new()),
            windows: RefCell::new(Vec::new()),
            expires: Cell::new(None),
        }))
    }
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
    pub(crate) fn device(&self) -> &Rc<DeviceHandle> {
        &self.device
    }
    pub(crate) fn probe_window(&self) -> Result<()> {
        let mut key = 0;
        let window =
            NonNull::new(unsafe { (self.device.api.window)(self.device.raw.as_ptr(), &mut key) })
                .ok_or(Error::Unavailable)?;
        if unsafe { (self.device.api.window_free)(window.as_ptr()) } != 0 {
            std::mem::forget(self.device.clone());
            return Err(Error::Io);
        }
        Ok(())
    }
    pub(crate) fn ready(&self) -> bool {
        self.connected.get() && !self.stopped.get() && !self.terminating.get()
    }
    pub(crate) fn stopped(&self) -> bool {
        self.stopped.get()
    }
    pub(crate) fn expire_at(&self, deadline: std::time::Instant) {
        self.expires.set(Some(deadline));
    }
    fn reserve(
        &self,
        opcode: u32,
        region: Option<Rc<Region>>,
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
    fn submitted(&self, id: u64, rc: i32) -> Result<()> {
        if rc != 0 {
            // Exactly one WR was posted, so a synchronous rejection posts none.
            if let Some(pending) = self.pending.borrow_mut().remove(&id) {
                pending.finish(Err(Error::Io));
            }
            return Err(Error::Io);
        }
        Ok(())
    }
    pub(crate) fn bind(&self, region: Rc<Region>) -> Result<(Rc<Window>, Ticket)> {
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
            )
        };
        if let Err(error) = self.submitted(id, rc) {
            self.stop()?;
            return Err(error);
        }
        Ok((window, ticket))
    }
    pub(crate) fn invalidate(&self, window: Rc<Window>) -> Result<Ticket> {
        if !self.windows.borrow().iter().any(|w| Rc::ptr_eq(w, &window)) {
            return Err(Error::InvalidRequest);
        }
        let (id, ticket) = self.reserve(6, None, Some(window.clone()))?; // IBV_WC_LOCAL_INV
        let rc = unsafe { (self.device.api.invalidate)(self.raw.as_ptr(), window.key, id) };
        self.submitted(id, rc)?;
        Ok(ticket)
    }
    pub(crate) fn write(&self, region: Rc<Region>, address: u64, key: u32) -> Result<Ticket> {
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
            (self.device.api.write)(self.raw.as_ptr(), region.raw.as_ptr(), address, key, id)
        };
        self.submitted(id, rc)?;
        Ok(ticket)
    }
    /// Nonblocking and bounded to 32 CQEs per call. Call on the owning reactor.
    pub fn progress(&self) -> Result<usize> {
        if self.stopped.get() {
            return Ok(0);
        }
        if self.terminating.get() {
            self.stop()?;
            return Err(Error::Cancelled);
        }
        if self
            .expires
            .get()
            .is_some_and(|deadline| std::time::Instant::now() >= deadline)
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
            // A failed/unknown completion requires a terminal fence before release.
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
    /// Terminal DMA fence. Success permits reuse; failure retains all ownership.
    pub fn stop(&self) -> Result<()> {
        if self.stopped.get() {
            return Ok(());
        }
        self.terminating.set(true);
        if unsafe { (self.device.api.stop)(self.raw.as_ptr()) } != 0 {
            return Err(Error::Io);
        }
        self.stopped.set(true);
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
impl Drop for QueuePairHandle {
    fn drop(&mut self) {
        if self.stop().is_err() {
            std::mem::forget(std::mem::take(self.pending.get_mut()));
            std::mem::forget(std::mem::take(self.windows.get_mut()));
            std::mem::forget(self.device.clone());
            return;
        }
        if unsafe { (self.device.api.qp_free)(self.raw.as_ptr()) } != 0 {
            std::mem::forget(self.device.clone());
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
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
    fn optional_runtime_never_fabricates_a_device() {
        match Verbs.discover() {
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
#[path = "verbs_tests.rs"]
mod lifetime_tests;
