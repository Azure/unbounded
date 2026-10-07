//! A fake RDMA fabric for tests. No hardware or sysfs needed.
//!
//! It implements the same C functions as `native/verbs.c`, so the real
//! wrappers in `ffi.rs` run on top of it unchanged.
//!
//! # Basics
//!
//! 1. Create one [`Simulation`] per test thread.
//! 2. Give each node its own view with [`Simulation::with_devices`].
//! 3. Call [`Simulation::enter`] before building that node's `NativeService`.
//!    Handles keep working after the scope ends.
//! 4. Script failures with [`Simulation::fault`] or [`Simulation::reject`].
//! 5. Check what happened with [`Simulation::trace`].
//!
//! Addresses seen on the wire are fake; they are never host pointers.
//! Device names must be unique within a discovery view, even for different ports.
//! A remote QP must already exist in this fabric before its peer connects to it.
use super::*;

use std::collections::VecDeque;

thread_local! {
    static CURRENT: RefCell<Option<Simulation>> = const { RefCell::new(None) };
}

/// A fabric shared by every node on one test thread. Deterministic.
///
/// Clones and `with_devices` views share the same fabric; only the devices
/// each view discovers differ.
#[derive(Clone)]
pub struct Simulation {
    world: Rc<RefCell<World>>,

    devices: Vec<Device>,
}

/// A fake port that discovery will report.
#[derive(Clone, Debug)]
pub struct Device {
    pub name: String,

    pub gid: [u8; 16],

    pub port: u8,

    pub numa_node: Option<usize>,
}

impl Device {
    /// Port 1 of `name`, with no NUMA node.
    pub fn new(name: impl Into<String>, gid: [u8; 16]) -> Self {
        Self {
            name: name.into(),
            gid,
            port: 1,
            numa_node: None,
        }
    }
}

/// Scope guard from [`Simulation::enter`]. Restores the previous view on drop.
pub struct Environment {
    previous: Option<Simulation>,
}

impl Drop for Environment {
    /// Put back the previous view.
    fn drop(&mut self) {
        CURRENT.with(|current| *current.borrow_mut() = self.previous.take());
    }
}

/// One C function call. Used in traces and to target faults.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Operation {
    Discover,

    Open,

    Close,

    Qp,

    Connect,

    Stop,

    QpFree,

    Register,

    Deregister,

    Window,

    WindowFree,

    Bind,

    Invalidate,

    Write,

    Poll,
}

/// A one-shot failure for the next matching call.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Fault {
    /// Fail the call itself. Nothing changes.
    Reject,

    /// Hold a Bind, Invalidate, or Write for this many polls.
    Delay(usize),

    /// Complete a Bind, Invalidate, or Write with this error status (nonzero).
    /// Its effect is skipped.
    Completion(u32),
}

/// One entry in the trace: a call or a completion.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct Event {
    pub sequence: u64,

    pub operation: Operation,

    pub resource: u64,

    pub work_id: Option<u64>,

    /// 0 is success, negative is a rejected call, positive is a completion error.
    /// Includes validation failures. Successful discovery and polls use 0, not counts.
    pub result: i64,

    pub completion: bool,
}

/// Everything in the fabric: resources, scripted faults, and the trace.
#[derive(Default)]
struct World {
    next: u32,

    resources: BTreeMap<u32, Resource>,

    faults: VecDeque<(Operation, Option<u32>, Fault)>,

    rejects: Vec<(Operation, Option<u32>)>,

    trace: Vec<Event>,

    sequence: u64,
}

/// A live device, queue pair, region, or window.
enum Resource {
    Device(Device),

    Qp {
        device: u32,

        local: Option<Endpoint>,

        remote: Option<Endpoint>,

        stopped: bool,

        work: VecDeque<Work>,
    },

    Region {
        device: u32,

        bytes: Box<[u8]>,

        address: u64,
    },

    Window {
        device: u32,

        grant: Option<Grant>,
    },
}

/// A bound window: which QP may write, into which region, how many bytes.
#[derive(Clone, Copy)]
struct Grant {
    qp: u32,

    region: u32,

    length: u32,
}

/// Queued work on a QP, with its remaining delay and final status.
struct Work {
    id: u64,

    delay: usize,

    status: u32,

    action: Action,
}

/// What the work does when it completes successfully.
#[derive(Clone, Copy)]
enum Action {
    Bind {
        window: u32,

        region: u32,

        length: u32,
    },

    Invalidate {
        key: u32,
    },

    Write {
        region: u32,

        address: u64,

        key: u32,

        length: u32,
    },
}

impl Action {
    /// The matching `Operation`.
    fn operation(&self) -> Operation {
        match self {
            Self::Bind { .. } => Operation::Bind,
            Self::Invalidate { .. } => Operation::Invalidate,
            Self::Write { .. } => Operation::Write,
        }
    }

    /// The completion opcode `ffi.rs` expects.
    fn opcode(&self) -> u32 {
        match self {
            Self::Bind { .. } => 5,
            Self::Invalidate { .. } => 6,
            Self::Write { .. } => 1,
        }
    }
}

impl Simulation {
    /// Turn on or off a lasting rule that fails every matching call.
    /// Applies to teardown calls too.
    pub fn reject(&self, operation: Operation, qpn: Option<u32>, enabled: bool) {
        let mut world = self.world.borrow_mut();
        world.rejects.retain(|rule| *rule != (operation, qpn));
        if enabled {
            world.rejects.push((operation, qpn));
        }
    }

    /// An empty fabric.
    pub fn new() -> Self {
        Self {
            world: Rc::new(RefCell::new(World::default())),
            devices: Vec::new(),
        }
    }

    /// A view of the same fabric that discovers `devices`.
    /// Rejects more than 64 ports, bad names, zero GIDs or ports, and duplicate
    /// device names, including entries with the same name but different ports.
    pub fn with_devices(&self, devices: Vec<Device>) -> Result<Self> {
        if devices.len() > 64
            || devices.iter().any(|d| {
                d.name.is_empty()
                    || d.name.len() >= 64
                    || d.name.as_bytes().contains(&0)
                    || d.gid == [0; 16]
                    || d.port == 0
            })
            || devices
                .iter()
                .enumerate()
                .any(|(i, d)| devices[..i].iter().any(|other| other.name == d.name))
        {
            return Err(Error::InvalidConfiguration);
        }
        Ok(Self {
            world: self.world.clone(),
            devices,
        })
    }

    /// Make this view current on this thread until the guard drops.
    pub fn enter(&self) -> Environment {
        Environment {
            previous: CURRENT.with(|current| current.replace(Some(self.clone()))),
        }
    }

    /// Fail the next matching call on any resource.
    pub fn fault(&self, operation: Operation, fault: Fault) {
        self.fault_on(operation, None, fault);
    }

    /// Fail the next matching call, optionally only on one QP.
    /// Panics on `Delay`/`Completion` for non-work calls, or `Completion(0)`.
    pub fn fault_on(&self, operation: Operation, qpn: Option<u32>, fault: Fault) {
        assert!(
            matches!(fault, Fault::Reject)
                || matches!(
                    operation,
                    Operation::Bind | Operation::Invalidate | Operation::Write
                )
        );
        assert!(!matches!(fault, Fault::Completion(0)));
        self.world
            .borrow_mut()
            .faults
            .push_back((operation, qpn, fault));
    }

    /// Copy of the trace so far.
    pub fn trace(&self) -> Vec<Event> {
        self.world.borrow().trace.clone()
    }

    /// Take the trace and clear it. Sequence numbers keep counting.
    pub fn take_trace(&self) -> Vec<Event> {
        std::mem::take(&mut self.world.borrow_mut().trace)
    }

    /// Resources not yet freed, including ones leaked on purpose.
    pub fn live_resources(&self) -> usize {
        self.world.borrow().resources.len()
    }

    /// One-shot faults not yet used. Ignores `reject` rules.
    pub fn pending_faults(&self) -> usize {
        self.world.borrow().faults.len()
    }
}

impl Default for Simulation {
    /// An empty fabric.
    fn default() -> Self {
        Self::new()
    }
}

/// The view current on this thread, if any.
pub(crate) fn current() -> Option<Simulation> {
    CURRENT.with(|current| current.borrow().clone())
}

/// True if `api` is this fake.
pub(super) fn is_api(api: &Api) -> bool {
    std::ptr::fn_addr_eq(
        api.open,
        open as unsafe extern "C" fn(*const c_char) -> *mut c_void,
    )
}

/// The function table `ffi.rs` uses in place of the C library.
pub(super) fn api() -> Rc<Api> {
    Rc::new(Api {
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
    })
}

/// The opaque pointer handed to `ffi.rs`. Holds the fabric and a resource id.
/// The fabric owns the resource; `ffi.rs` decides when to free it.
struct Handle {
    sim: Simulation,

    id: u32,
}

/// Return no handle for null. The caller keeps any non-null handle alive.
unsafe fn handle<'a>(raw: *mut c_void) -> Option<&'a Handle> {
    unsafe { raw.cast::<Handle>().as_ref() }
}

/// Add a resource and return a new handle to it.
fn allocate(sim: &Simulation, resource: Resource) -> *mut c_void {
    let id = sim.world.borrow_mut().insert(resource);
    Box::into_raw(Box::new(Handle {
        sim: sim.clone(),
        id,
    }))
    .cast()
}

/// The NUMA node set on a device.
pub(super) fn numa_node(raw: NonNull<c_void>) -> Option<usize> {
    let h = unsafe { handle(raw.as_ptr()) }?;
    match h.sim.world.borrow().resources.get(&h.id) {
        Some(Resource::Device(d)) => d.numa_node,
        _ => None,
    }
}

/// A region's fake wire address.
pub(super) fn address(raw: NonNull<c_void>) -> u64 {
    let Some(h) = (unsafe { handle(raw.as_ptr()) }) else {
        return 0;
    };
    match h.sim.world.borrow().resources.get(&h.id) {
        Some(Resource::Region { address, .. }) => *address,
        _ => 0,
    }
}

impl World {
    /// Store a resource under a new id. Ids fit in 24 bits, like real QPNs.
    fn insert(&mut self, resource: Resource) -> u32 {
        self.next = self
            .next
            .checked_add(1)
            .expect("simulation resource IDs exhausted");
        assert!(self.next <= 0xffffff, "simulation QPN space exhausted");
        self.resources.insert(self.next, resource);
        self.next
    }

    /// Add an event to the trace.
    fn record(
        &mut self,
        operation: Operation,
        resource: u32,
        work_id: Option<u64>,
        result: i64,
        completion: bool,
    ) {
        self.trace.push(Event {
            sequence: self.sequence,
            operation,
            resource: resource.into(),
            work_id,
            result,
            completion,
        });
        self.sequence += 1;
    }

    /// The fault for this call: a `reject` rule first, else the next one-shot fault.
    fn fault(&mut self, operation: Operation, resource: u32) -> Option<Fault> {
        if self
            .rejects
            .iter()
            .any(|(op, target)| *op == operation && target.is_none_or(|id| id == resource))
        {
            return Some(Fault::Reject);
        }
        let i = self.faults.iter().position(|(op, target, _)| {
            *op == operation && target.is_none_or(|id| id == resource)
        })?;
        self.faults.remove(i).map(|(_, _, fault)| fault)
    }

    /// Check a scripted rejection and trace only the failed call.
    fn rejected(&mut self, operation: Operation, resource: u32) -> bool {
        let rejected = self.fault(operation, resource) == Some(Fault::Reject);
        if rejected {
            self.record(operation, resource, None, -1, false);
        }
        rejected
    }

    /// Run a call unless rejected, then trace its final result.
    fn call(
        &mut self,
        operation: Operation,
        resource: u32,
        run: impl FnOnce(&mut Self) -> c_int,
    ) -> c_int {
        if self.rejected(operation, resource) {
            return -1;
        }
        let result = run(self);
        self.record(operation, resource, None, i64::from(result), false);
        result
    }

    /// The device a resource belongs to.
    fn device(&self, id: u32) -> Option<u32> {
        match self.resources.get(&id)? {
            Resource::Device(_) => Some(id),
            Resource::Qp { device, .. }
            | Resource::Region { device, .. }
            | Resource::Window { device, .. } => Some(*device),
        }
    }

    /// True if a QP is connected and not stopped.
    fn ready(&self, id: u32) -> bool {
        matches!(
            self.resources.get(&id),
            Some(Resource::Qp {
                local: Some(_),
                remote: Some(_),
                stopped: false,
                ..
            })
        )
    }

    /// True if two QPs are connected to each other and not stopped.
    fn paired(&self, id: u32, peer: u32) -> bool {
        match (self.resources.get(&id), self.resources.get(&peer)) {
            (
                Some(Resource::Qp {
                    local: Some(a),
                    remote: Some(b),
                    stopped: false,
                    ..
                }),
                Some(Resource::Qp {
                    local: Some(c),
                    remote: Some(d),
                    stopped: false,
                    ..
                }),
            ) => a == d && b == c,
            _ => false,
        }
    }

    /// Check and queue one Bind, Invalidate, or Write. Returns -1 if rejected.
    fn post(&mut self, qp: u32, id: u64, action: Action) -> c_int {
        let op = action.operation();
        let fault = self.fault(op, qp);
        let valid = self.ready(qp)
            && match &action {
                Action::Bind {
                    window,
                    region,
                    length,
                } => {
                    self.device(*window) == self.device(qp)
                        && self.device(*region) == self.device(qp)
                        && matches!(
                            self.resources.get(window),
                            Some(Resource::Window { grant: None, .. })
                        )
                        && self.region_fits(*region, *length)
                }
                Action::Write { region, length, .. } => {
                    self.device(*region) == self.device(qp) && self.region_fits(*region, *length)
                }
                Action::Invalidate { .. } => true,
            };
        if !valid || fault == Some(Fault::Reject) {
            self.record(op, qp, Some(id), -1, false);
            return -1;
        }
        let work = Work {
            id,
            action,
            delay: match fault {
                Some(Fault::Delay(n)) => n,
                _ => 0,
            },
            status: match fault {
                Some(Fault::Completion(status)) => status,
                _ => 0,
            },
        };
        if let Some(Resource::Qp { work: pending, .. }) = self.resources.get_mut(&qp) {
            pending.push_back(work);
        }
        self.record(op, qp, Some(id), 0, false);
        0
    }

    /// True if `length` is nonzero and fits in the region.
    fn region_fits(&self, region: u32, length: u32) -> bool {
        matches!(self.resources.get(&region), Some(Resource::Region { bytes, .. }) if length > 0 && length as usize <= bytes.len())
    }

    /// Run queued work. Returns 0, or a verbs error status with nothing copied.
    fn execute(&mut self, qp: u32, action: &Action) -> u32 {
        match *action {
            Action::Bind {
                window,
                region,
                length,
            } => {
                if let Some(Resource::Window { grant, .. }) = self.resources.get_mut(&window)
                    && grant.is_none()
                {
                    *grant = Some(Grant { qp, region, length });
                    return 0;
                }
            }
            Action::Invalidate { key } => {
                if let Some(Resource::Window { grant, .. }) = self.resources.get_mut(&key)
                    && grant.is_some_and(|g| g.qp == qp)
                {
                    *grant = None;
                    return 0;
                }
            }
            Action::Write {
                region,
                address,
                key,
                length,
            } => {
                let Some(Resource::Window {
                    grant: Some(grant), ..
                }) = self.resources.get(&key)
                else {
                    return 10;
                };
                let grant = *grant;
                if !self.paired(qp, grant.qp) {
                    return 10;
                }
                let Some(Resource::Region {
                    address: base,
                    bytes,
                    ..
                }) = self.resources.get(&grant.region)
                else {
                    return 10;
                };
                let Some(offset) = address.checked_sub(*base) else {
                    return 10;
                };
                if offset
                    .checked_add(length.into())
                    .is_none_or(|end| end > grant.length as u64 || end > bytes.len() as u64)
                {
                    return 10;
                }
                let Some(Resource::Region { bytes: source, .. }) = self.resources.get(&region)
                else {
                    return 4;
                };
                let copy = source[..length as usize].to_vec();
                let Some(Resource::Region { bytes: target, .. }) =
                    self.resources.get_mut(&grant.region)
                else {
                    return 10;
                };
                target[offset as usize..offset as usize + copy.len()].copy_from_slice(&copy);
                return 0;
            }
        }
        10 // IBV_WC_REM_ACCESS_ERR
    }
}

/// Report the current view's ports. Fails if they do not fit in `capacity`.
unsafe extern "C" fn discover(out: *mut Port, capacity: u32) -> c_int {
    let Some(sim) = current() else {
        return -1;
    };
    if sim.world.borrow_mut().rejected(Operation::Discover, 0) {
        return -1;
    }
    if sim.devices.len() > capacity as usize {
        sim.world
            .borrow_mut()
            .record(Operation::Discover, 0, None, -1, false);
        return -1;
    }
    for (i, device) in sim.devices.iter().enumerate() {
        let mut port = Port {
            name: [0; 64],
            gid: device.gid,
            mtu: 3,
            lid: 0,
            port: device.port,
            link_layer: 2,
        };
        for (dst, src) in port.name.iter_mut().zip(device.name.bytes()) {
            *dst = src as c_char;
        }
        unsafe {
            *out.add(i) = port;
        }
    }
    sim.world
        .borrow_mut()
        .record(Operation::Discover, 0, None, 0, false);
    sim.devices.len() as c_int
}

/// Open a device by name from the current view.
unsafe extern "C" fn open(name: *const c_char) -> *mut c_void {
    let Some(sim) = current() else {
        return std::ptr::null_mut();
    };
    if sim.world.borrow_mut().rejected(Operation::Open, 0) {
        return std::ptr::null_mut();
    }
    let name = unsafe { CStr::from_ptr(name) }.to_bytes();
    let Some(device) = sim.devices.iter().find(|d| d.name.as_bytes() == name) else {
        sim.world
            .borrow_mut()
            .record(Operation::Open, 0, None, -1, false);
        return std::ptr::null_mut();
    };
    let raw = allocate(&sim, Resource::Device(device.clone()));
    sim.world
        .borrow_mut()
        .record(Operation::Open, 0, None, 0, false);
    raw
}

/// Create a QP on the device's port. Writes its QPN.
unsafe extern "C" fn qp(device: *mut c_void, port: u8, _: u32, qpn: *mut u32) -> *mut c_void {
    let Some(h) = (unsafe { handle(device) }) else {
        return std::ptr::null_mut();
    };
    let mut world = h.sim.world.borrow_mut();
    if world.rejected(Operation::Qp, h.id) {
        return std::ptr::null_mut();
    }
    if !matches!(world.resources.get(&h.id), Some(Resource::Device(d)) if d.port == port) {
        world.record(Operation::Qp, h.id, None, -1, false);
        return std::ptr::null_mut();
    }
    drop(world);
    let raw = allocate(
        &h.sim,
        Resource::Qp {
            device: h.id,
            local: None,
            remote: None,
            stopped: false,
            work: VecDeque::new(),
        },
    );
    unsafe {
        *qpn = handle(raw).unwrap().id;
    }
    h.sim
        .world
        .borrow_mut()
        .record(Operation::Qp, h.id, None, 0, false);
    raw
}

/// Connect a QP once. Both endpoints must match real fabric resources.
unsafe extern "C" fn connect(
    raw: *mut c_void,
    local: *const Endpoint,
    remote: *const Endpoint,
) -> c_int {
    let Some(h) = (unsafe { handle(raw) }) else {
        return -1;
    };
    let (local, remote) = unsafe { (*local, *remote) };
    let mut world = h.sim.world.borrow_mut();
    world.call(Operation::Connect, h.id, |world| {
        if local.qpn != h.id || local.validate().is_err() || remote.validate().is_err() {
            return -1;
        }
        let Some(device_id) = world.device(h.id) else {
            return -1;
        };
        if !matches!(
            world.resources.get(&device_id),
            Some(Resource::Device(d)) if d.gid == local.gid && d.port == local.port
        ) {
            return -1;
        }
        let Some(remote_device) = world.device(remote.qpn) else {
            return -1;
        };
        if !matches!(
            world.resources.get(&remote.qpn),
            Some(Resource::Qp { stopped: false, .. })
        ) {
            return -1;
        }
        if !matches!(
            world.resources.get(&remote_device),
            Some(Resource::Device(d)) if d.gid == remote.gid && d.port == remote.port
        ) {
            return -1;
        }
        let Some(Resource::Qp {
            local: ours,
            remote: theirs,
            stopped: false,
            ..
        }) = world.resources.get_mut(&h.id)
        else {
            return -1;
        };
        if ours.is_some() {
            return -1;
        }
        *ours = Some(local);
        *theirs = Some(remote);
        0
    })
}

/// Stop a QP: drop its queued work and unbind its windows.
unsafe extern "C" fn stop(raw: *mut c_void) -> c_int {
    let Some(h) = (unsafe { handle(raw) }) else {
        return -1;
    };
    let mut world = h.sim.world.borrow_mut();
    world.call(Operation::Stop, h.id, |world| {
        let Some(Resource::Qp { stopped, work, .. }) = world.resources.get_mut(&h.id) else {
            return -1;
        };
        *stopped = true;
        work.clear();
        for resource in world.resources.values_mut() {
            if let Resource::Window { grant, .. } = resource
                && grant.is_some_and(|g| g.qp == h.id)
            {
                *grant = None;
            }
        }
        0
    })
}

/// Free a resource. Fails if anything still uses it.
unsafe fn free(raw: *mut c_void, operation: Operation) -> c_int {
    let Some(h) = (unsafe { handle(raw) }) else {
        return -1;
    };
    let mut world = h.sim.world.borrow_mut();
    let result = world.call(operation, h.id, |world| {
        let referenced = world.resources.iter().any(|(id, resource)| {
            if *id == h.id {
                return false;
            }
            match resource {
                Resource::Qp { device, work, .. } => {
                    *device == h.id
                        || work.iter().any(|w| match w.action {
                            Action::Bind { window, region, .. } => window == h.id || region == h.id,
                            Action::Invalidate { key } => key == h.id,
                            Action::Write { region, .. } => region == h.id,
                        })
                }
                Resource::Region { device, .. } => *device == h.id,
                Resource::Window { device, grant } => {
                    *device == h.id || grant.is_some_and(|g| g.region == h.id || g.qp == h.id)
                }
                _ => false,
            }
        });
        if referenced
            || matches!(
                world.resources.get(&h.id),
                Some(Resource::Window { grant: Some(_), .. })
            )
            || matches!(
                world.resources.get(&h.id),
                Some(Resource::Qp { work, .. }) if !work.is_empty()
            )
        {
            return -1;
        }
        world.resources.remove(&h.id);
        0
    });
    if result != 0 {
        return result;
    }
    drop(world);
    unsafe {
        drop(Box::from_raw(raw.cast::<Handle>()));
    }
    0
}

/// Close a device. Fails while anything on it is alive.
unsafe extern "C" fn close(raw: *mut c_void) -> c_int {
    unsafe { free(raw, Operation::Close) }
}

/// Free a QP. Fails while it has work or bound windows.
unsafe extern "C" fn qp_free(raw: *mut c_void) -> c_int {
    unsafe { free(raw, Operation::QpFree) }
}

/// Free a region. Fails while work or a window uses it.
unsafe extern "C" fn deregister(raw: *mut c_void) -> c_int {
    unsafe { free(raw, Operation::Deregister) }
}

/// Free a window. Fails while bound or used by queued work.
unsafe extern "C" fn window_free(raw: *mut c_void) -> c_int {
    unsafe { free(raw, Operation::WindowFree) }
}

/// Create a zeroed region with a fake wire address.
unsafe extern "C" fn register(device: *mut c_void, length: u32) -> *mut c_void {
    let Some(h) = (unsafe { handle(device) }) else {
        return std::ptr::null_mut();
    };
    let mut world = h.sim.world.borrow_mut();
    if length == 0 {
        world.record(Operation::Register, h.id, None, -1, false);
        return std::ptr::null_mut();
    }
    if world.rejected(Operation::Register, h.id) {
        return std::ptr::null_mut();
    }
    let address = ((world.next as u64) + 1) << 32;
    drop(world);
    let raw = allocate(
        &h.sim,
        Resource::Region {
            device: h.id,
            bytes: vec![0; length as usize].into_boxed_slice(),
            address,
        },
    );
    h.sim
        .world
        .borrow_mut()
        .record(Operation::Register, h.id, None, 0, false);
    raw
}

/// Pointer to a region's bytes. Valid until the region is freed.
unsafe extern "C" fn bytes(raw: *mut c_void) -> *mut u8 {
    let Some(h) = (unsafe { handle(raw) }) else {
        return std::ptr::null_mut();
    };
    match h.sim.world.borrow_mut().resources.get_mut(&h.id) {
        Some(Resource::Region { bytes, .. }) => bytes.as_mut_ptr(),
        _ => std::ptr::null_mut(),
    }
}

/// Create an unbound window. Its id is its key.
unsafe extern "C" fn window(device: *mut c_void, key: *mut u32) -> *mut c_void {
    let Some(h) = (unsafe { handle(device) }) else {
        return std::ptr::null_mut();
    };
    if h.sim.world.borrow_mut().rejected(Operation::Window, h.id) {
        return std::ptr::null_mut();
    }
    let raw = allocate(
        &h.sim,
        Resource::Window {
            device: h.id,
            grant: None,
        },
    );
    unsafe {
        *key = handle(raw).unwrap().id;
    }
    h.sim
        .world
        .borrow_mut()
        .record(Operation::Window, h.id, None, 0, false);
    raw
}

/// Queue a bind. QP, window, and region must share one fabric.
unsafe extern "C" fn bind(
    qp: *mut c_void,
    window: *mut c_void,
    region: *mut c_void,
    key: u32,
    id: u64,
    length: u32,
) -> c_int {
    let (Some(q), Some(w), Some(r)) = (unsafe { (handle(qp), handle(window), handle(region)) })
    else {
        return -1;
    };
    if !Rc::ptr_eq(&q.sim.world, &w.sim.world)
        || !Rc::ptr_eq(&q.sim.world, &r.sim.world)
        || key != w.id
    {
        q.sim
            .world
            .borrow_mut()
            .record(Operation::Bind, q.id, Some(id), -1, false);
        return -1;
    }
    q.sim.world.borrow_mut().post(
        q.id,
        id,
        Action::Bind {
            window: w.id,
            region: r.id,
            length,
        },
    )
}

/// Queue an invalidate. Checked when it completes.
unsafe extern "C" fn invalidate(qp: *mut c_void, key: u32, id: u64) -> c_int {
    let Some(q) = (unsafe { handle(qp) }) else {
        return -1;
    };
    q.sim
        .world
        .borrow_mut()
        .post(q.id, id, Action::Invalidate { key })
}

/// Queue a write. The remote key is checked when it completes.
unsafe extern "C" fn write(
    qp: *mut c_void,
    region: *mut c_void,
    address: u64,
    key: u32,
    id: u64,
    length: u32,
) -> c_int {
    let (Some(q), Some(r)) = (unsafe { (handle(qp), handle(region)) }) else {
        return -1;
    };
    if !Rc::ptr_eq(&q.sim.world, &r.sim.world) {
        q.sim
            .world
            .borrow_mut()
            .record(Operation::Write, q.id, Some(id), -1, false);
        return -1;
    }
    q.sim.world.borrow_mut().post(
        q.id,
        id,
        Action::Write {
            region: r.id,
            address,
            key,
            length,
        },
    )
}

/// Run up to 32 queued requests in order. Stops at a delay or a failure.
unsafe extern "C" fn poll(qp: *mut c_void, out: *mut Completion, capacity: u32) -> c_int {
    let Some(q) = (unsafe { handle(qp) }) else {
        return -1;
    };
    let mut world = q.sim.world.borrow_mut();
    if world.rejected(Operation::Poll, q.id) {
        return -1;
    }
    let mut count = 0;
    while count < capacity.min(32) {
        let Some(Resource::Qp {
            work,
            stopped: false,
            ..
        }) = world.resources.get_mut(&q.id)
        else {
            break;
        };
        let Some(first) = work.front_mut() else {
            break;
        };
        if first.delay > 0 {
            first.delay -= 1;
            break;
        }
        let pending = work.pop_front().unwrap();
        let status = if pending.status == 0 {
            world.execute(q.id, &pending.action)
        } else {
            pending.status
        };
        world.record(
            pending.action.operation(),
            q.id,
            Some(pending.id),
            status as i64,
            true,
        );
        unsafe {
            *out.add(count as usize) = Completion {
                id: pending.id,
                status,
                opcode: pending.action.opcode(),
            };
        }
        count += 1;
        // After a failure, nothing more runs. `ffi.rs` sees it and stops the QP.
        if status != 0 {
            break;
        }
    }
    world.record(Operation::Poll, q.id, None, 0, false);
    count as c_int
}

#[cfg(test)]
mod tests {
    //! Connected DMA, failure ownership, discovery, and safe proxy integration tests.
    use super::*;

    use crate::test_guard::ready;

    use std::task::{Context, Poll};

    /// Check every event since the last assertion, including order and identity.
    fn events(sim: &Simulation, expected: &[(Operation, u32, Option<u64>, i64, bool)]) {
        let trace = sim.take_trace();
        let first = sim.world.borrow().sequence - trace.len() as u64;
        let expected: Vec<_> = expected
            .iter()
            .enumerate()
            .map(
                |(i, &(operation, resource, work_id, result, completion))| Event {
                    sequence: first + i as u64,
                    operation,
                    resource: resource.into(),
                    work_id,
                    result,
                    completion,
                },
            )
            .collect();
        assert_eq!(trace, expected);
    }

    /// Every handle consumer rejects null before reading or writing through it.
    fn null_handle_calls_fail(raw: *mut c_void) {
        assert!(raw.is_null());
        let mut output = 123;
        let endpoint = Endpoint {
            gid: [1; 16],
            qpn: 1,
            psn: 0,
            mtu: 3,
            lid: 0,
            port: 1,
            link_layer: 2,
        };
        let mut completion = Completion {
            id: 42,
            status: 43,
            opcode: 44,
        };
        unsafe {
            assert!(handle(raw).is_none());
            assert!(qp(raw, 1, 1, &mut output).is_null());
            assert!(register(raw, 32).is_null());
            assert!(window(raw, &mut output).is_null());
            assert!(bytes(raw).is_null());
            assert_eq!(connect(raw, &endpoint, &endpoint), -1);
            assert_eq!(stop(raw), -1);
            assert_eq!(bind(raw, raw, raw, 1, 2, 3), -1);
            assert_eq!(invalidate(raw, 1, 2), -1);
            assert_eq!(write(raw, raw, 1, 2, 3, 4), -1);
            assert_eq!(poll(raw, &mut completion, 1), -1);
            assert_eq!(close(raw), -1);
            assert_eq!(qp_free(raw), -1);
            assert_eq!(deregister(raw), -1);
            assert_eq!(window_free(raw), -1);
        }
        assert_eq!(output, 123);
        assert_eq!(
            (completion.id, completion.status, completion.opcode),
            (42, 43, 44)
        );
    }

    #[test]
    /// All eight allocation failure paths can be passed back without a null access.
    fn allocation_failures_reject_null_handles_without_side_effects() {
        assert!(current().is_none());
        null_handle_calls_fail(unsafe { open(c"sim0".as_ptr()) });
        let sim = Simulation::new()
            .with_devices(vec![Device::new("sim0", [1; 16])])
            .unwrap();
        let _scope = sim.enter();
        unsafe {
            let device = open(c"sim0".as_ptr());
            assert!(handle(device).is_some());
            let mut output = 123;
            let mut failures = vec![
                open(c"missing".as_ptr()),
                qp(device, 2, 1, &mut output),
                register(device, 0),
            ];
            sim.fault(Operation::Open, Fault::Reject);
            failures.push(open(c"sim0".as_ptr()));
            sim.fault(Operation::Qp, Fault::Reject);
            failures.push(qp(device, 1, 1, &mut output));
            sim.fault(Operation::Register, Fault::Reject);
            failures.push(register(device, 32));
            sim.fault(Operation::Window, Fault::Reject);
            failures.push(window(device, &mut output));
            assert_eq!(output, 123);
            assert_eq!(sim.pending_faults(), 0);
            let trace = sim.trace();
            sim.fault(Operation::Stop, Fault::Reject);
            for raw in failures {
                null_handle_calls_fail(raw);
                assert_eq!(sim.live_resources(), 1);
                assert_eq!(sim.trace(), trace);
                assert_eq!(sim.pending_faults(), 1);
            }
            assert_eq!(close(device), 0);
        }
        assert_eq!(sim.live_resources(), 0);
    }

    #[test]
    /// A null secondary handle cannot post work or consume a scripted fault.
    fn posts_reject_each_null_handle_without_side_effects() {
        let sim = Simulation::new();
        {
            let (sender, receiver, source, target) = connected(&sim);
            let (window, binding) = receiver.bind(target.clone()).unwrap();
            receiver.progress().unwrap();
            assert_eq!(binding.result(), Some(Ok(())));
            let q = sender.raw.as_ptr();
            let w = window.raw.as_ptr();
            let r = source.raw.as_ptr();
            let null = std::ptr::null_mut();
            let trace = sim.trace();
            let live = sim.live_resources();
            sim.fault(Operation::Bind, Fault::Reject);
            sim.fault(Operation::Write, Fault::Reject);
            unsafe {
                for (q, w, r) in [(null, w, r), (q, null, r), (q, w, null)] {
                    assert_eq!(bind(q, w, r, window.key, 100, 17), -1);
                }
                for (q, r) in [(null, r), (q, null)] {
                    assert_eq!(write(q, r, target.address(), window.key, 101, 17), -1);
                }
            }
            assert_eq!(sim.trace(), trace);
            assert_eq!(sim.live_resources(), live);
            assert_eq!(sim.pending_faults(), 2);
            assert_eq!(sender.progress(), Ok(0));
            receiver.stop().unwrap();
            assert_eq!(source.copy_to().unwrap(), [0xa5; 17]);
            assert_eq!(target.copy_to().unwrap(), [0; 17]);
        }
        assert_eq!(sim.live_resources(), 0);
    }

    #[test]
    /// Validation and scripted failures each emit one event and allocate nothing.
    fn allocation_events_report_final_outcomes() {
        let sim = Simulation::new()
            .with_devices(vec![Device::new("sim0", [1; 16])])
            .unwrap();
        let _scope = sim.enter();
        unsafe {
            assert_eq!(discover(std::ptr::null_mut(), 0), -1);
            events(&sim, &[(Operation::Discover, 0, None, -1, false)]);
            let mut ports = std::mem::MaybeUninit::<Port>::uninit();
            assert_eq!(discover(ports.as_mut_ptr(), 1), 1);
            events(&sim, &[(Operation::Discover, 0, None, 0, false)]);
            assert!(open(c"missing".as_ptr()).is_null());
            events(&sim, &[(Operation::Open, 0, None, -1, false)]);
            assert_eq!(sim.live_resources(), 0);
            let device = open(c"sim0".as_ptr());
            assert!(!device.is_null());
            events(&sim, &[(Operation::Open, 0, None, 0, false)]);
            let d = handle(device).unwrap().id;
            let mut id = 0;
            assert!(qp(device, 2, 1, &mut id).is_null());
            assert_eq!(id, 0);
            events(&sim, &[(Operation::Qp, d, None, -1, false)]);
            assert!(register(device, 0).is_null());
            events(&sim, &[(Operation::Register, d, None, -1, false)]);
            assert_eq!(sim.live_resources(), 1);
            for operation in [
                Operation::Discover,
                Operation::Open,
                Operation::Qp,
                Operation::Register,
                Operation::Window,
            ] {
                sim.fault(operation, Fault::Reject);
                let resource = match operation {
                    Operation::Discover => {
                        assert_eq!(discover(ports.as_mut_ptr(), 1), -1);
                        0
                    }
                    Operation::Open => {
                        assert!(open(c"sim0".as_ptr()).is_null());
                        0
                    }
                    Operation::Qp => {
                        assert!(qp(device, 1, 1, &mut id).is_null());
                        d
                    }
                    Operation::Register => {
                        assert!(register(device, 32).is_null());
                        d
                    }
                    Operation::Window => {
                        assert!(window(device, &mut id).is_null());
                        d
                    }
                    _ => unreachable!(),
                };
                events(&sim, &[(operation, resource, None, -1, false)]);
                assert_eq!(sim.live_resources(), 1);
                assert_eq!(sim.pending_faults(), 0);
            }
            assert_eq!(stop(device), -1);
            events(&sim, &[(Operation::Stop, d, None, -1, false)]);
            assert_eq!(close(device), 0);
            events(&sim, &[(Operation::Close, d, None, 0, false)]);
        }
        assert_eq!(sim.live_resources(), 0);
    }

    #[test]
    /// Failed connects leave the QP reusable; failed frees retain every handle.
    fn connect_and_free_events_preserve_ownership_on_validation_failure() {
        let sim = Simulation::new()
            .with_devices(vec![Device::new("sim0", [1; 16])])
            .unwrap();
        let _scope = sim.enter();
        unsafe {
            let device = open(c"sim0".as_ptr());
            let d = handle(device).unwrap().id;
            let mut qpn = 0;
            let q = qp(device, 1, 1, &mut qpn);
            let local = Endpoint {
                gid: [1; 16],
                qpn,
                psn: 0,
                mtu: 3,
                lid: 0,
                port: 1,
                link_layer: 2,
            };
            let peer = qp(device, 1, 1, &mut qpn);
            let remote = Endpoint { qpn, ..local };
            let region = register(device, 32);
            let r = handle(region).unwrap().id;
            let mut key = 0;
            let w = window(device, &mut key);
            events(
                &sim,
                &[
                    (Operation::Open, 0, None, 0, false),
                    (Operation::Qp, d, None, 0, false),
                    (Operation::Qp, d, None, 0, false),
                    (Operation::Register, d, None, 0, false),
                    (Operation::Window, d, None, 0, false),
                ],
            );
            for bad in [
                Endpoint {
                    qpn: 0xffffff,
                    ..remote
                },
                Endpoint {
                    gid: [2; 16],
                    ..remote
                },
                Endpoint { port: 0, ..remote },
                Endpoint { qpn: r, ..remote },
            ] {
                assert_eq!(connect(q, &local, &bad), -1);
                events(&sim, &[(Operation::Connect, local.qpn, None, -1, false)]);
                assert!(!sim.world.borrow().ready(local.qpn));
                assert_eq!(sim.live_resources(), 5);
            }
            sim.fault(Operation::Connect, Fault::Reject);
            assert_eq!(connect(q, &local, &remote), -1);
            events(&sim, &[(Operation::Connect, local.qpn, None, -1, false)]);
            assert!(!sim.world.borrow().ready(local.qpn));
            assert_eq!(connect(q, &local, &remote), 0);
            events(&sim, &[(Operation::Connect, local.qpn, None, 0, false)]);
            assert!(sim.world.borrow().ready(local.qpn));
            assert_eq!(connect(q, &local, &remote), -1);
            events(&sim, &[(Operation::Connect, local.qpn, None, -1, false)]);
            assert_eq!(bind(q, w, region, key, 7, 17), 0);
            events(&sim, &[(Operation::Bind, local.qpn, Some(7), 0, false)]);
            for completed in [false, true] {
                if completed {
                    let mut completion = Completion::default();
                    assert_eq!(poll(q, &mut completion, 1), 1);
                    assert_eq!(completion.status, 0);
                    events(
                        &sim,
                        &[
                            (Operation::Bind, local.qpn, Some(7), 0, true),
                            (Operation::Poll, local.qpn, None, 0, false),
                        ],
                    );
                }
                for (raw, operation) in [
                    (device, Operation::Close),
                    (q, Operation::QpFree),
                    (region, Operation::Deregister),
                    (w, Operation::WindowFree),
                ] {
                    let id = handle(raw).unwrap().id;
                    assert_eq!(free(raw, operation), -1);
                    events(&sim, &[(operation, id, None, -1, false)]);
                    assert_eq!(handle(raw).unwrap().id, id);
                    assert!(sim.world.borrow().resources.contains_key(&id));
                    assert_eq!(sim.live_resources(), 5);
                }
            }
            sim.fault(Operation::Stop, Fault::Reject);
            assert_eq!(stop(q), -1);
            events(&sim, &[(Operation::Stop, local.qpn, None, -1, false)]);
            assert!(sim.world.borrow().ready(local.qpn));
            assert_eq!(stop(q), 0);
            events(&sim, &[(Operation::Stop, local.qpn, None, 0, false)]);
            assert!(!sim.world.borrow().ready(local.qpn));
            for (raw, operation) in [
                (w, Operation::WindowFree),
                (region, Operation::Deregister),
                (q, Operation::QpFree),
                (peer, Operation::QpFree),
                (device, Operation::Close),
            ] {
                let id = handle(raw).unwrap().id;
                let live = sim.live_resources();
                sim.fault(operation, Fault::Reject);
                assert_eq!(free(raw, operation), -1);
                events(&sim, &[(operation, id, None, -1, false)]);
                assert_eq!(sim.live_resources(), live);
                assert_eq!(free(raw, operation), 0);
                events(&sim, &[(operation, id, None, 0, false)]);
                assert_eq!(sim.live_resources(), live - 1);
                assert!(!sim.world.borrow().resources.contains_key(&id));
            }
        }
        assert_eq!(sim.live_resources(), 0);
        assert_eq!(sim.pending_faults(), 0);
    }

    #[test]
    /// Early post failures and completion failures have distinct, accurate events.
    fn post_and_poll_events_include_validation_and_completion_failures() {
        let sim = Simulation::new();
        let foreign = Simulation::new();
        {
            let (sender, receiver, source, target) = connected(&sim);
            let (_, _, foreign_region, _) = connected(&foreign);
            let (w, binding) = receiver.bind(target.clone()).unwrap();
            receiver.progress().unwrap();
            assert_eq!(binding.result(), Some(Ok(())));
            sim.take_trace();
            foreign.take_trace();
            let q = sender.raw.as_ptr();
            let qpn = sender.endpoint.qpn;
            unsafe {
                for (region, key) in [
                    (foreign_region.raw.as_ptr(), w.key),
                    (source.raw.as_ptr(), w.key + 1),
                ] {
                    assert_eq!(bind(q, w.raw.as_ptr(), region, key, 41, 17), -1);
                    events(&sim, &[(Operation::Bind, qpn, Some(41), -1, false)]);
                }
                assert_eq!(
                    write(
                        q,
                        foreign_region.raw.as_ptr(),
                        target.address(),
                        w.key,
                        42,
                        17
                    ),
                    -1
                );
                events(&sim, &[(Operation::Write, qpn, Some(42), -1, false)]);
                assert_eq!(
                    write(q, source.raw.as_ptr(), target.address(), w.key, 43, 33),
                    -1
                );
                events(&sim, &[(Operation::Write, qpn, Some(43), -1, false)]);
                assert_eq!(poll(q, std::ptr::null_mut(), 0), 0);
                events(&sim, &[(Operation::Poll, qpn, None, 0, false)]);
                for (operation, id) in [(Operation::Write, 44), (Operation::Invalidate, 45)] {
                    let result = if operation == Operation::Write {
                        write(
                            q,
                            source.raw.as_ptr(),
                            target.address(),
                            w.key + 100,
                            id,
                            17,
                        )
                    } else {
                        invalidate(q, w.key + 100, id)
                    };
                    assert_eq!(result, 0);
                    events(&sim, &[(operation, qpn, Some(id), 0, false)]);
                    sim.fault(Operation::Poll, Fault::Reject);
                    let mut completion = Completion::default();
                    assert_eq!(poll(q, &mut completion, 1), -1);
                    events(&sim, &[(Operation::Poll, qpn, None, -1, false)]);
                    assert_eq!(poll(q, &mut completion, 1), 1);
                    assert_eq!(completion.id, id);
                    assert_eq!(completion.status, 10);
                    events(
                        &sim,
                        &[
                            (operation, qpn, Some(id), 10, true),
                            (Operation::Poll, qpn, None, 0, false),
                        ],
                    );
                }
            }
            events(&foreign, &[]);
            assert_eq!(source.copy_to().unwrap(), [0xa5; 17]);
            receiver.stop().unwrap();
            assert_eq!(target.copy_to().unwrap(), [0; 17]);
        }
        assert_eq!(sim.live_resources(), 0);
        assert_eq!(foreign.live_resources(), 0);
    }

    /// Build connected native peers with 17-byte transfers in 32-byte allocations.
    fn connected(
        sim: &Simulation,
    ) -> (
        Rc<NativeQueuePair>,
        Rc<NativeQueuePair>,
        Rc<NativeRegion>,
        Rc<NativeRegion>,
    ) {
        let node = sim
            .with_devices(vec![Device::new("sim0", [1; 16])])
            .unwrap();
        let _scope = node.enter();
        let device = Rc::new(super::super::discover().unwrap().remove(0));
        let sender = NativeQueuePair::new(device.clone()).unwrap();
        let receiver = NativeQueuePair::new(device.clone()).unwrap();
        sender.connect(receiver.endpoint).unwrap();
        receiver.connect(sender.endpoint).unwrap();
        let source = NativeRegion::new(device.clone(), 32, lifetime_tests::quota().0).unwrap();
        let target = NativeRegion::new(device, 32, lifetime_tests::quota().0).unwrap();
        source.resize(17).unwrap();
        target.resize(17).unwrap();
        source.copy_from(&[0xa5; 17]).unwrap();
        (sender, receiver, source, target)
    }

    #[test]
    /// Delayed writes require completion and invalidated keys reject subsequent DMA.
    fn connected_copy_waits_for_completion_and_invalidation_revokes_key() {
        let sim = Simulation::new();
        {
            let (sender, receiver, source, target) = connected(&sim);
            let (window, bind) = receiver.bind(target.clone()).unwrap();
            receiver.progress().unwrap();
            assert_eq!(bind.result(), Some(Ok(())));
            sim.fault(Operation::Write, Fault::Delay(2));
            let write = sender
                .write(source.clone(), target.address(), window.key)
                .unwrap();
            assert_eq!(sender.progress(), Ok(0));
            assert_eq!(sender.progress(), Ok(0));
            assert_eq!(write.result(), None);
            assert_eq!(source.copy_to(), Err(Error::Unavailable));
            assert_eq!(target.copy_to(), Err(Error::Unavailable));
            assert_eq!(sender.progress(), Ok(1));
            assert_eq!(write.result(), Some(Ok(())));
            let invalidation = receiver.invalidate(window.clone()).unwrap();
            receiver.progress().unwrap();
            assert_eq!(invalidation.result(), Some(Ok(())));
            // The key is revoked even before the stronger application stop fence.
            let stale = sender.write(source, target.address(), window.key).unwrap();
            assert_eq!(sender.progress(), Err(Error::Io));
            assert_eq!(stale.result(), Some(Err(Error::Cancelled)));
            receiver.stop().unwrap();
            assert_eq!(target.copy_to().unwrap(), [0xa5; 17]);
        }
        assert_eq!(sim.live_resources(), 0);
    }

    #[test]
    /// Both completion orders retain the old grant until a successful terminal fence.
    fn fallback_cannot_reuse_quarantined_region_in_either_completion_order() {
        for remote_first in [false, true] {
            let sim = Simulation::new();
            let (quota, charged) = lifetime_tests::quota();
            {
                let (sender, receiver, source, _) = connected(&sim);
                let target = NativeRegion::new(sender.device().clone(), 32, quota).unwrap();
                target.resize(17).unwrap();
                let (window, bind) = receiver.bind(target.clone()).unwrap();
                assert_eq!(receiver.progress(), Ok(1));
                assert_eq!(bind.result(), Some(Ok(())));
                let fallback_sender = NativeQueuePair::new(sender.device().clone()).unwrap();
                let fallback_receiver = NativeQueuePair::new(sender.device().clone()).unwrap();
                fallback_sender.connect(fallback_receiver.endpoint).unwrap();
                fallback_receiver.connect(fallback_sender.endpoint).unwrap();
                sim.fault(Operation::Write, Fault::Delay(1));
                let write = sender
                    .write(source.clone(), target.address(), window.key)
                    .unwrap();
                assert_eq!(sender.progress(), Ok(0));
                // Revocation requested, but no remote fence has completed yet.
                sim.fault(Operation::Invalidate, Fault::Delay(1));
                let invalidation = receiver.invalidate(window.clone()).unwrap();
                assert_eq!(receiver.progress(), Ok(0));
                assert!(matches!(
                    fallback_receiver.bind(target.clone()),
                    Err(Error::InvalidRequest)
                ));
                assert_eq!(target.copy_to(), Err(Error::Unavailable));
                if remote_first {
                    assert_eq!(receiver.progress(), Ok(1));
                    assert_eq!(invalidation.result(), Some(Ok(())));
                    assert_eq!(write.result(), None);
                    assert_eq!(source.copy_to(), Err(Error::Unavailable));
                } else {
                    // A queued DMA can arrive after the invalidate request.
                    assert_eq!(sender.progress(), Ok(1));
                    assert_eq!(write.result(), Some(Ok(())));
                    assert_eq!(source.copy_to().unwrap(), [0xa5; 17]);
                    assert_eq!(invalidation.result(), None);
                }
                assert_eq!(charged.get(), 1);
                assert_eq!(target.copy_to(), Err(Error::Unavailable));
                assert!(matches!(
                    fallback_receiver.bind(target.clone()),
                    Err(Error::InvalidRequest)
                ));
                if remote_first {
                    assert_eq!(sender.progress(), Err(Error::Io));
                    assert_eq!(write.result(), Some(Err(Error::Cancelled)));
                } else {
                    assert_eq!(receiver.progress(), Ok(1));
                    assert_eq!(invalidation.result(), Some(Ok(())));
                }
                // Production requires the terminal QP fence even after invalidation.
                assert_eq!(target.copy_to(), Err(Error::Unavailable));
                receiver.stop().unwrap();
                let expected = if remote_first { [0; 17] } else { [0xa5; 17] };
                assert_eq!(target.copy_to().unwrap(), expected);
                let (fresh_window, fresh_bind) = fallback_receiver.bind(target.clone()).unwrap();
                assert_ne!(fresh_window.key, window.key);
                fallback_receiver.progress().unwrap();
                assert_eq!(fresh_bind.result(), Some(Ok(())));
                let stale = fallback_sender
                    .write(source, target.address(), window.key)
                    .unwrap();
                assert_eq!(fallback_sender.progress(), Err(Error::Io));
                assert_eq!(stale.result(), Some(Err(Error::Cancelled)));
                fallback_receiver.stop().unwrap();
                assert_eq!(
                    target.copy_to().unwrap(),
                    expected,
                    "old key cannot modify the reused destination"
                );
                assert_eq!(charged.get(), 1);
            }
            assert_eq!(charged.get(), 0);
            assert_eq!(sim.live_resources(), 0);
        }
    }

    #[test]
    /// Inclusive expiry blocks admission but failed fencing still permits late DMA.
    fn expiry_is_inclusive_but_not_a_remote_fence_and_faults_are_ordered() {
        use std::time::Duration;

        use uring_runtime::environment::{SimulationClock, now};

        for failed_stop in [false, true] {
            let clock = SimulationClock::new(7);
            let environment = clock.environment(0);
            let _clock = environment.enter();
            let sim = Simulation::new();
            {
                let (sender, receiver, source, target) = connected(&sim);
                let (window, bind) = receiver.bind(target.clone()).unwrap();
                receiver.progress().unwrap();
                assert_eq!(bind.result(), Some(Ok(())));
                receiver.expire_at(now() + Duration::from_secs(2));
                clock.advance(Duration::from_secs(2) - Duration::from_nanos(1));
                assert_eq!(receiver.progress(), Ok(0));
                assert!(receiver.ready());
                sim.fault(Operation::Invalidate, Fault::Delay(2));
                let invalidation = receiver.invalidate(window.clone()).unwrap();
                assert_eq!(receiver.progress(), Ok(0));
                clock.advance(Duration::from_nanos(1));
                if failed_stop {
                    sim.fault_on(Operation::Stop, Some(receiver.endpoint.qpn), Fault::Reject);
                    assert_eq!(receiver.progress(), Err(Error::Io));
                    assert!(!receiver.ready());
                    assert!(!receiver.stopped());
                    assert_eq!(invalidation.result(), None);
                    assert_eq!(target.copy_to(), Err(Error::Unavailable));
                    assert!(matches!(
                        receiver.invalidate(window.clone()),
                        Err(Error::Unavailable)
                    ));
                    // Expiry blocks local admission, but a failed stop cannot revoke DMA.
                    let late = sender
                        .write(source.clone(), target.address(), window.key)
                        .unwrap();
                    assert_eq!(sender.progress(), Ok(1));
                    assert_eq!(late.result(), Some(Ok(())));
                    assert_eq!(target.copy_to(), Err(Error::Unavailable));
                    assert_eq!(receiver.progress(), Err(Error::Cancelled));
                } else {
                    assert_eq!(receiver.progress(), Err(Error::DeadlineExceeded));
                }
                assert!(receiver.stopped());
                assert_eq!(invalidation.result(), Some(Err(Error::Cancelled)));
                assert_eq!(receiver.progress(), Ok(0));
                let expected = if failed_stop { [0xa5; 17] } else { [0; 17] };
                assert_eq!(target.copy_to().unwrap(), expected);
                let stale = sender.write(source, target.address(), window.key).unwrap();
                assert_eq!(sender.progress(), Err(Error::Io));
                assert_eq!(stale.result(), Some(Err(Error::Cancelled)));
                assert_eq!(target.copy_to().unwrap(), expected);
            }
            assert_eq!(sim.pending_faults(), 0);
            assert_eq!(sim.live_resources(), 0);
        }
    }

    #[test]
    /// Invalid capabilities, ranges, and peer pairing cannot modify the destination.
    fn wrong_key_address_length_and_peer_cannot_write() {
        for case in 0..4 {
            let sim = Simulation::new();
            {
                let (sender, receiver, source, target) = connected(&sim);
                let (window, _) = receiver.bind(target.clone()).unwrap();
                receiver.progress().unwrap();
                let (address, key) = match case {
                    0 => (target.address(), window.key + 100),
                    1 => (target.address() - 1, window.key),
                    2 => (target.address() + 1, window.key),
                    _ => (target.address(), window.key),
                };
                let stranger = NativeQueuePair::new(sender.device().clone()).unwrap();
                stranger.connect(receiver.endpoint).unwrap();
                let writer = if case == 3 { &stranger } else { &sender };
                writer.write(source, address, key).unwrap();
                assert_eq!(writer.progress(), Err(Error::Io));
                receiver.stop().unwrap();
                assert_eq!(target.copy_to().unwrap(), [0; 17]);
            }
            assert_eq!(sim.live_resources(), 0);
        }
    }

    #[test]
    /// Failed invalidation and fencing retain remote access until fencing is retried.
    fn failed_invalidation_and_stop_keep_live_grant_until_retry() {
        let sim = Simulation::new();
        {
            let (sender, receiver, source, target) = connected(&sim);
            let (window, _) = receiver.bind(target.clone()).unwrap();
            receiver.progress().unwrap();
            sim.fault_on(
                Operation::Invalidate,
                Some(receiver.endpoint.qpn),
                Fault::Completion(5),
            );
            let invalidation = receiver.invalidate(window.clone()).unwrap();
            sim.fault_on(Operation::Stop, Some(receiver.endpoint.qpn), Fault::Reject);
            assert_eq!(receiver.progress(), Err(Error::Io));
            assert_eq!(invalidation.result(), None);
            assert_eq!(target.copy_to(), Err(Error::Unavailable));
            // Failed fencing must model a real late remote DMA, not merely an error.
            let write = sender.write(source, target.address(), window.key).unwrap();
            sender.progress().unwrap();
            assert_eq!(write.result(), Some(Ok(())));
            receiver.stop().unwrap();
            assert_eq!(invalidation.result(), Some(Err(Error::Cancelled)));
            assert_eq!(target.copy_to().unwrap(), [0xa5; 17]);
        }
        assert_eq!(sim.live_resources(), 0);
    }

    #[test]
    /// Rejected posts leave the source idle and terminal fencing cancels delayed DMA.
    fn stop_cancels_delayed_dma_and_post_rejection_keeps_source_reusable() {
        let sim = Simulation::new();
        {
            let (sender, receiver, source, target) = connected(&sim);
            let (window, _) = receiver.bind(target.clone()).unwrap();
            receiver.progress().unwrap();
            sim.fault(Operation::Write, Fault::Reject);
            assert!(
                sender
                    .write(source.clone(), target.address(), window.key)
                    .is_err()
            );
            assert_eq!(source.copy_to().unwrap(), [0xa5; 17]);
            sim.fault(Operation::Write, Fault::Delay(3));
            let pending = sender.write(source, target.address(), window.key).unwrap();
            sender.stop().unwrap();
            assert_eq!(pending.result(), Some(Err(Error::Cancelled)));
            assert_eq!(sender.progress(), Ok(0));
            receiver.stop().unwrap();
            assert_eq!(target.copy_to().unwrap(), [0; 17]);
        }
        assert_eq!(sim.live_resources(), 0);
    }

    #[test]
    /// A poll error with failed fencing keeps source memory busy until a retry.
    fn polling_failure_retains_pending_source_until_stop_succeeds() {
        let sim = Simulation::new();
        {
            let (sender, receiver, source, target) = connected(&sim);
            let (window, _) = receiver.bind(target.clone()).unwrap();
            receiver.progress().unwrap();
            let pending = sender
                .write(source.clone(), target.address(), window.key)
                .unwrap();
            sim.fault_on(Operation::Poll, Some(sender.endpoint.qpn), Fault::Reject);
            sim.fault_on(Operation::Stop, Some(sender.endpoint.qpn), Fault::Reject);
            assert_eq!(sender.progress(), Err(Error::Io));
            assert_eq!(pending.result(), None);
            assert_eq!(source.copy_to(), Err(Error::Unavailable));
            sender.stop().unwrap();
            assert_eq!(pending.result(), Some(Err(Error::Cancelled)));
            receiver.stop().unwrap();
            assert_eq!(target.copy_to().unwrap(), [0; 17]);
        }
        assert_eq!(sim.live_resources(), 0);
    }

    #[test]
    /// Invalid discovery views and scripted discovery failures never fabricate ports.
    fn discovery_failure_and_invalid_device_definitions_fail_closed() {
        let sim = Simulation::new();
        assert!(sim.with_devices(vec![Device::new("bad", [0; 16])]).is_err());
        assert!(
            sim.with_devices(vec![Device::new("bad\0name", [1; 16])])
                .is_err()
        );
        assert!(
            sim.with_devices(vec![
                Device::new("sim0", [1; 16]),
                Device::new("sim0", [2; 16])
            ])
            .is_err()
        );
        let node = sim
            .with_devices(vec![Device::new("sim0", [1; 16])])
            .unwrap();
        let _environment = node.enter();
        sim.fault(Operation::Discover, Fault::Reject);
        assert!(matches!(super::super::discover(), Err(Error::Unavailable)));
        sim.fault(Operation::Open, Fault::Reject);
        assert!(super::super::discover().unwrap().is_empty());
        assert_eq!(super::super::discover().unwrap().len(), 1);
        assert_eq!(sim.live_resources(), 0);
    }

    #[test]
    /// Nested discovery scopes restore correctly and repeated runs reproduce addresses.
    fn discovery_scopes_restore_and_virtual_addresses_replay() {
        /// Run one scoped discovery and allocation sequence for replay comparison.
        fn run() -> (u64, Vec<Event>) {
            let sim = Simulation::new();
            let node = sim
                .with_devices(vec![Device {
                    numa_node: Some(7),
                    ..Device::new("sim0", [1; 16])
                }])
                .unwrap();
            let scope = node.enter();
            let device = Rc::new(super::super::discover().unwrap().remove(0));
            assert_eq!(device.numa_node(), Some(7));
            {
                let _empty = sim.enter();
                assert!(super::super::discover().unwrap().is_empty());
            }
            assert_eq!(super::super::discover().unwrap().len(), 1);
            let region = NativeRegion::new(device, 32, lifetime_tests::quota().0).unwrap();
            let address = region.address();
            drop(scope);
            drop(region);
            assert_eq!(sim.live_resources(), 0);
            (address, sim.trace())
        }

        assert_eq!(run(), run());
    }

    #[test]
    /// Paired native services discover separate nodes and copy through safe poll APIs.
    fn native_services_activate_discovered_nodes_and_copy_through_io_proxies() {
        use crate::{Configuration, NativeService, QueuePairHandle, Region, pair};

        let sim = Simulation::new();
        let mut charges = Vec::new();
        let mut nodes = Vec::new();
        for n in 1..=2 {
            let node = sim
                .with_devices(vec![Device::new("sim0", [n; 16])])
                .unwrap();
            let (io, port) = pair(1).unwrap();
            let mut native = {
                let _environment = node.enter();
                NativeService::new(port)
            };
            let devices = Rc::new(io);
            let (guard, charge) = crate::test_guard::guard();
            charges.push(charge);
            futures::executor::block_on(devices.configure(Configuration {
                discover: true,
                guards: vec![guard],
                bytes: 4096,
                selector: Box::new(move |ports| {
                    assert_eq!(ports.len(), 1);
                    assert_eq!(ports[0].device, "sim0");
                    assert_eq!(ports[0].port, 1);
                    assert_eq!(ports[0].gid, [n; 16]);
                    Ok(vec![(0, 0)])
                }),
            }))
            .unwrap();
            let mut activation = Box::pin(std::future::poll_fn(|_| {
                devices.activation().map_or(Poll::Pending, Poll::Ready)
            }));
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            assert!(activation.as_mut().poll(&mut cx).is_pending());
            // Discovery still selects this service's node after its scope has exited.
            native.poll_budgeted(1).unwrap();
            assert!(activation.as_mut().poll(&mut cx).is_pending());
            native.poll_budgeted(1).unwrap();
            assert!(matches!(
                activation.as_mut().poll(&mut cx),
                Poll::Ready(Ok(_))
            ));
            drop(activation);
            let qp = ready(QueuePairHandle::poll_new(devices.device(0), None)).unwrap();
            nodes.push((native, devices, qp));
        }
        let (mut left, devices_left, sender) = nodes.remove(0);
        let (mut right, devices_right, receiver) = nodes.remove(0);
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        ready(sender.poll_connect(receiver.endpoint)).unwrap();
        ready(receiver.poll_connect(sender.endpoint)).unwrap();
        left.poll_budgeted(1).unwrap();
        right.poll_budgeted(1).unwrap();
        sender.progress().unwrap();
        receiver.progress().unwrap();
        assert!(sender.ready() && receiver.ready());
        let target = ready(Region::poll_acquire(&receiver, 17)).unwrap();
        let (window, bind) = ready(receiver.poll_bind(target.clone())).unwrap();
        right.poll_budgeted(1).unwrap();
        right.poll_budgeted(1).unwrap();
        assert_eq!(bind.result(), Some(Ok(())));
        let source = ready(Region::poll_acquire(&sender, 17)).unwrap();
        ready(source.poll_copy_from(&[0xa5; 17])).unwrap();
        let write =
            ready(sender.poll_write(source.clone(), window.address.get(), window.key.get()))
                .unwrap();
        left.poll_budgeted(1).unwrap();
        left.poll_budgeted(1).unwrap();
        assert_eq!(write.result(), Some(Ok(())));
        let invalidation = ready(receiver.poll_invalidate(window.clone(), &mut cx)).unwrap();
        right.poll_budgeted(1).unwrap();
        right.poll_budgeted(1).unwrap();
        assert_eq!(invalidation.result(), Some(Ok(())));
        receiver.stop();
        right.poll_budgeted(1).unwrap();
        assert_eq!(ready(target.poll_copy_to(&mut cx)).unwrap(), [0xa5; 17]);
        drop((
            source,
            target,
            write,
            bind,
            invalidation,
            window,
            sender,
            receiver,
        ));
        devices_left.close();
        devices_right.close();
        left.poll_budgeted(1).unwrap();
        right.poll_budgeted(1).unwrap();
        assert!(left.drained() && right.drained());
        assert!(charges.iter().all(|charge| charge.get() == 0));
        assert_eq!(sim.live_resources(), 0);
    }
}
