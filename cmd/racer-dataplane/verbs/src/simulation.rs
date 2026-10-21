//! A fake RDMA fabric for tests. No hardware or sysfs needed.
//!
//! It implements the same C functions as `native/verbs.c`, so the real
//! wrappers in `ffi.rs` run on top of it unchanged.
//!
//! # Basics
//!
//! 1. Create one [`Simulation`] per test thread.
//! 2. Give each node its own view with [`Simulation::with_devices`].
//! 3. Call [`Simulation::enter`] before building that node's [`crate::Owner`].
//!    Handles keep working after the scope ends.
//! 4. Script failures with [`Simulation::fault`] or [`Simulation::reject`].
//!    Delivery faults ([`Fault::Drop`], [`Fault::Reorder`],
//!    [`Fault::DuplicateImmediate`], [`Fault::ByteLength`]) model a broken
//!    fabric for `Send` and `WriteImm`.
//! 5. Check what happened with [`Simulation::trace`].
//!
//! Addresses seen on the wire are fake; they are never host pointers.
//! Device names must be unique within a discovery view, even for different ports.
//! A remote QP must already exist in this fabric before its peer connects to it.
//!
//! # Verbs model
//!
//! - Send work runs in post order when the posting QP polls. Receive
//!   completions land on the peer's CQ and are reported by the peer's poll
//!   before its own send work.
//! - `Send` and `WriteImm` consume the peer's oldest posted receive. With none
//!   posted the sender fails with `IBV_WC_RNR_RETRY_EXC_ERR` (`rnr_retry` is
//!   0). A `WriteImm` whose window check passed has already placed its data;
//!   real multi-packet writes can do the same before the RNR.
//! - A remote access error, RNR, or receive length error moves both QPs to the
//!   error state. Any failed completion moves its own QP there. A QP in the
//!   error state flushes every posted work request with `IBV_WC_WR_FLUSH_ERR`.
//! - Until a QP is stopped (destroyed), plain RDMA writes through a live grant
//!   still land, even in the error state, so tests stay conservative about
//!   late DMA.
//! - Window keys are `index << 8 | generation`, like `ibv_inc_rkey`. A bind
//!   must use a key with the window's index that differs from the last one.
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

    WriteImm,

    Send,

    PostRecv,

    Poll,
}

/// A one-shot failure for the next matching call.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Fault {
    /// Fail the call itself. Nothing changes.
    Reject,

    /// Hold a Bind, Invalidate, or Write for this many polls.
    Delay(usize),

    /// Complete a send-queue request with this error status (nonzero).
    /// Its effect is skipped and the QP moves to the error state.
    Completion(u32),

    /// `Send` or `WriteImm`: complete successfully on the sender but deliver
    /// nothing to the peer.
    Drop,

    /// `Send` or `WriteImm`: deliver after the next send-queue request on the
    /// same QP. Held until one is posted.
    Reorder,

    /// `Send` or `WriteImm`: deliver the receive completion twice, consuming
    /// two receives.
    DuplicateImmediate,

    /// `Send` or `WriteImm`: report this `byte_len` on the receive completion.
    ByteLength(u32),
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

    /// Fail binds on regions registered without remote write.
    require_mr_remote_write: bool,

    /// Devices report no implicit on-demand paging.
    no_implicit_odp: bool,

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

        /// In the error state: posted work flushes.
        error: bool,

        work: VecDeque<Work>,

        recvs: VecDeque<Recv>,

        /// Delivered receive completions not yet polled.
        cq: VecDeque<Completion>,
    },

    Region {
        device: u32,

        bytes: Box<[u8]>,

        address: u64,

        /// `RDMA_VERBS_MR_*` bits.
        access: u32,
    },

    Window {
        device: u32,

        /// Last key used for a bind, if any.
        key: Option<u32>,

        grant: Option<Grant>,
    },
}

/// A bound window: which QP may write, into which region, which bytes.
#[derive(Clone, Copy)]
struct Grant {
    qp: u32,

    region: u32,

    key: u32,

    offset: u64,

    length: u32,
}

/// A posted receive.
#[derive(Clone, Copy)]
struct Recv {
    id: u64,

    region: u32,

    offset: u64,

    length: u32,
}

/// Queued work on a QP, with its remaining delay, final status, and any
/// delivery fault.
struct Work {
    id: u64,

    delay: usize,

    status: u32,

    delivery: Option<Fault>,

    action: Action,
}

/// The window index in a key.
fn window_of(key: u32) -> u32 {
    key >> 8
}

/// `RDMA_VERBS_MR_PAGES`.
const MR_PAGES: u32 = 1;

/// `RDMA_VERBS_MR_CONTROL`.
const MR_CONTROL: u32 = 2;

/// `RDMA_VERBS_MR_REMOTE_WRITE`.
const MR_REMOTE_WRITE: u32 = 4;

/// An implicit on-demand paging region over this process's address space.
/// Offsets are virtual addresses; it is only a local write or send source.
const MR_IMPLICIT: u32 = 8;

/// Verbs completion statuses the fabric produces.
mod wc {
    pub(super) const LOC_LEN_ERR: u32 = 1;

    pub(super) const LOC_PROT_ERR: u32 = 4;

    pub(super) const WR_FLUSH_ERR: u32 = 5;

    pub(super) const MW_BIND_ERR: u32 = 6;

    pub(super) const REM_INV_REQ_ERR: u32 = 9;

    pub(super) const REM_ACCESS_ERR: u32 = 10;

    pub(super) const RETRY_EXC_ERR: u32 = 12;

    pub(super) const RNR_RETRY_EXC_ERR: u32 = 13;
}

/// What the work does when it completes successfully.
#[derive(Clone, Copy)]
enum Action {
    Bind {
        window: u32,

        region: u32,

        key: u32,

        offset: u64,

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

    WriteImm {
        region: u32,

        offset: u64,

        length: u32,

        address: u64,

        key: u32,

        imm: u32,
    },

    Send {
        region: u32,

        offset: u64,

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
            Self::WriteImm { .. } => Operation::WriteImm,
            Self::Send { .. } => Operation::Send,
        }
    }

    /// The completion opcode `ffi.rs` expects.
    fn opcode(&self) -> u32 {
        match self {
            Self::Bind { .. } => 5,
            Self::Invalidate { .. } => 6,
            Self::Write { .. } | Self::WriteImm { .. } => 1,
            Self::Send { .. } => 0,
        }
    }

    /// True if `id` is a region or window this work still uses.
    fn uses(&self, id: u32) -> bool {
        match *self {
            Self::Bind { window, region, .. } => window == id || region == id,
            Self::Invalidate { key } => window_of(key) == id,
            Self::Write { region, .. }
            | Self::WriteImm { region, .. }
            | Self::Send { region, .. } => region == id,
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
    /// Panics on `Delay`/`Completion` for non-send-queue calls, delivery
    /// faults for anything but `Send` and `WriteImm`, or `Completion(0)`.
    pub fn fault_on(&self, operation: Operation, qpn: Option<u32>, fault: Fault) {
        let send_queue = matches!(
            operation,
            Operation::Bind
                | Operation::Invalidate
                | Operation::Write
                | Operation::WriteImm
                | Operation::Send
        );
        let delivers = matches!(operation, Operation::WriteImm | Operation::Send);
        assert!(match fault {
            Fault::Reject => true,
            Fault::Delay(_) | Fault::Completion(_) => send_queue,
            Fault::Drop | Fault::Reorder | Fault::DuplicateImmediate | Fault::ByteLength(_) =>
                delivers,
        });
        assert!(!matches!(fault, Fault::Completion(0)));
        self.world
            .borrow_mut()
            .faults
            .push_back((operation, qpn, fault));
    }

    /// Choose whether binding a window on a region registered without
    /// `RDMA_VERBS_MR_REMOTE_WRITE` fails with `IBV_WC_MW_BIND_ERR`. Providers
    /// differ; the default (off) allows it.
    pub fn require_mr_remote_write(&self, enabled: bool) {
        self.world.borrow_mut().require_mr_remote_write = enabled;
    }

    /// Choose whether devices support implicit on-demand paging, the
    /// zero-copy write source. On by default.
    pub fn implicit_odp(&self, enabled: bool) {
        self.world.borrow_mut().no_implicit_odp = !enabled;
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
        register_implicit,
        deregister,
        bytes,
        window,
        window_free,
        bind,
        invalidate,
        write,
        write_imm,
        send,
        post_recv,
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

    /// Check and queue one send-queue request. Returns -1 if rejected.
    fn post(&mut self, qp: u32, id: u64, action: Action) -> c_int {
        let op = action.operation();
        let fault = self.fault(op, qp);
        let device = self.device(qp);
        let valid = self.ready(qp)
            && match action {
                Action::Bind {
                    window,
                    region,
                    key,
                    offset,
                    length,
                } => {
                    self.device(window) == device
                        && self.device(region) == device
                        && window_of(key) == window
                        && matches!(
                            self.resources.get(&window),
                            Some(Resource::Window { grant: None, key: last, .. }) if *last != Some(key)
                        )
                        && self.region_fits(region, offset, length)
                }
                Action::Write { region, length, .. } => {
                    self.device(region) == device && self.region_fits(region, 0, length)
                }
                Action::WriteImm {
                    region,
                    offset,
                    length,
                    ..
                }
                | Action::Send {
                    region,
                    offset,
                    length,
                } => self.device(region) == device && self.region_fits(region, offset, length),
                Action::Invalidate { .. } => true,
            };
        if !valid || fault == Some(Fault::Reject) {
            self.record(op, qp, Some(id), -1, false);
            return -1;
        }
        if let Action::Bind { window, key, .. } = action
            && let Some(Resource::Window { key: last, .. }) = self.resources.get_mut(&window)
        {
            *last = Some(key);
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
            delivery: fault.filter(|f| {
                matches!(
                    f,
                    Fault::Drop | Fault::Reorder | Fault::DuplicateImmediate | Fault::ByteLength(_)
                )
            }),
        };
        if let Some(Resource::Qp { work: pending, .. }) = self.resources.get_mut(&qp) {
            pending.push_back(work);
        }
        self.record(op, qp, Some(id), 0, false);
        0
    }

    /// Check and queue one receive. Allowed before connect, like verbs in
    /// the INIT state. Returns -1 if rejected.
    fn post_recv(&mut self, qp: u32, recv: Recv) -> c_int {
        let fault = self.fault(Operation::PostRecv, qp);
        let valid = matches!(
            self.resources.get(&qp),
            Some(Resource::Qp { stopped: false, .. })
        ) && self.device(recv.region) == self.device(qp)
            && !matches!(
                self.resources.get(&recv.region),
                Some(Resource::Region { access, .. }) if access & MR_IMPLICIT != 0
            )
            && self.region_fits(recv.region, recv.offset, recv.length);
        if !valid || fault == Some(Fault::Reject) {
            self.record(Operation::PostRecv, qp, Some(recv.id), -1, false);
            return -1;
        }
        if let Some(Resource::Qp { recvs, .. }) = self.resources.get_mut(&qp) {
            recvs.push_back(recv);
        }
        self.record(Operation::PostRecv, qp, Some(recv.id), 0, false);
        0
    }

    /// True if `[offset, offset + length)` is a nonempty range of the region.
    fn region_fits(&self, region: u32, offset: u64, length: u32) -> bool {
        if let Some(Resource::Region { access, .. }) = self.resources.get(&region)
            && access & MR_IMPLICIT != 0
        {
            return length > 0
                && offset
                    .checked_add(length.into())
                    .is_some_and(|end| usize::try_from(end).is_ok());
        }
        matches!(
            self.resources.get(&region),
            Some(Resource::Region { bytes, .. }) if length > 0
                && offset
                    .checked_add(length.into())
                    .is_some_and(|end| end <= bytes.len() as u64)
        )
    }

    /// The QP a QP is connected to.
    fn peer(&self, qp: u32) -> Option<u32> {
        match self.resources.get(&qp) {
            Some(Resource::Qp {
                remote: Some(remote),
                ..
            }) => Some(remote.qpn),
            _ => None,
        }
    }

    /// Move a QP to the error state.
    fn set_error(&mut self, qp: u32) {
        if let Some(Resource::Qp { error, .. }) = self.resources.get_mut(&qp) {
            *error = true;
        }
    }

    /// A copy of `length` bytes of a region at `offset`.
    fn read(&self, region: u32, offset: u64, length: u32) -> Option<Vec<u8>> {
        match self.resources.get(&region) {
            // SAFETY: an implicit region's offset is a live virtual address.
            // The verbs owner holds the source until this work completes or
            // the QP is destroyed, exactly as the NIC requires.
            Some(Resource::Region { access, .. }) if access & MR_IMPLICIT != 0 => Some(
                unsafe {
                    std::slice::from_raw_parts(offset as usize as *const u8, length as usize)
                }
                .to_vec(),
            ),
            Some(Resource::Region { bytes, .. }) => bytes
                .get(offset as usize..offset as usize + length as usize)
                .map(<[u8]>::to_vec),
            _ => None,
        }
    }

    /// Place an RDMA write through window `key` at the peer's `address`.
    /// Returns a verbs error status with nothing written.
    fn place(
        &mut self,
        qp: u32,
        data: &[u8],
        address: u64,
        key: u32,
    ) -> std::result::Result<(), u32> {
        let Some(Resource::Window {
            grant: Some(grant), ..
        }) = self.resources.get(&window_of(key))
        else {
            return Err(wc::REM_ACCESS_ERR);
        };
        let grant = *grant;
        if grant.key != key || !self.paired(qp, grant.qp) {
            return Err(wc::REM_ACCESS_ERR);
        }
        let Some(Resource::Region {
            address: base,
            bytes: target,
            ..
        }) = self.resources.get_mut(&grant.region)
        else {
            return Err(wc::REM_ACCESS_ERR);
        };
        let Some(offset) = address.checked_sub(*base) else {
            return Err(wc::REM_ACCESS_ERR);
        };
        let end = offset.checked_add(data.len() as u64);
        if offset < grant.offset
            || end.is_none_or(|end| {
                end > grant.offset + u64::from(grant.length) || end > target.len() as u64
            })
        {
            return Err(wc::REM_ACCESS_ERR);
        }
        target[offset as usize..offset as usize + data.len()].copy_from_slice(data);
        Ok(())
    }

    /// Consume the peer's receives for a `Send` (`imm` is `None`, `data` is
    /// the payload) or a `WriteImm`. Returns the sender's status.
    fn deliver(
        &mut self,
        qp: u32,
        work: &Work,
        imm: Option<u32>,
        data: Option<&[u8]>,
        length: u32,
    ) -> u32 {
        if work.delivery == Some(Fault::Drop) {
            return 0;
        }
        let Some(peer) = self.peer(qp).filter(|peer| self.paired(qp, *peer)) else {
            return wc::RETRY_EXC_ERR;
        };
        let copies = if work.delivery == Some(Fault::DuplicateImmediate) {
            2
        } else {
            1
        };
        for copy in 0..copies {
            let recv = match self.resources.get_mut(&peer) {
                Some(Resource::Qp {
                    recvs,
                    error: false,
                    ..
                }) => recvs.pop_front(),
                _ => None,
            };
            let Some(recv) = recv else {
                if copy > 0 {
                    break;
                }
                self.set_error(peer);
                return wc::RNR_RETRY_EXC_ERR;
            };
            let mut completion = Completion {
                id: recv.id,
                opcode: if imm.is_some() {
                    opcode::RECV_RDMA_WITH_IMM
                } else {
                    opcode::RECV
                },
                byte_len: match work.delivery {
                    Some(Fault::ByteLength(n)) => n,
                    _ => length,
                },
                imm: imm.unwrap_or(0),
                flags: u32::from(imm.is_some()) * WC_IMM,
                ..Completion::default()
            };
            let mut sender_status = 0;
            if let Some(data) = data {
                if data.len() > recv.length as usize {
                    completion = Completion {
                        id: recv.id,
                        status: wc::LOC_LEN_ERR,
                        opcode: u32::MAX,
                        ..Completion::default()
                    };
                    sender_status = wc::REM_INV_REQ_ERR;
                } else if let Some(Resource::Region { bytes, .. }) =
                    self.resources.get_mut(&recv.region)
                {
                    let start = recv.offset as usize;
                    bytes[start..start + data.len()].copy_from_slice(data);
                } else {
                    completion.status = wc::LOC_PROT_ERR;
                    completion.opcode = u32::MAX;
                    sender_status = wc::REM_INV_REQ_ERR;
                }
            }
            if let Some(Resource::Qp { cq, .. }) = self.resources.get_mut(&peer) {
                cq.push_back(completion);
            }
            if sender_status != 0 {
                self.set_error(peer);
                return sender_status;
            }
        }
        0
    }

    /// Run queued work. Returns 0, or a verbs error status. Remote errors
    /// also move the peer to the error state.
    fn execute(&mut self, qp: u32, work: &Work) -> u32 {
        match work.action {
            Action::Bind {
                window,
                region,
                key,
                offset,
                length,
            } => {
                let Some(Resource::Region { access, .. }) = self.resources.get(&region) else {
                    return wc::MW_BIND_ERR;
                };
                if access & MR_PAGES == 0
                    || (self.require_mr_remote_write && access & MR_REMOTE_WRITE == 0)
                {
                    return wc::MW_BIND_ERR;
                }
                if let Some(Resource::Window { grant, .. }) = self.resources.get_mut(&window)
                    && grant.is_none()
                {
                    *grant = Some(Grant {
                        qp,
                        region,
                        key,
                        offset,
                        length,
                    });
                    return 0;
                }
                wc::MW_BIND_ERR
            }
            Action::Invalidate { key } => {
                if let Some(Resource::Window { grant, .. }) =
                    self.resources.get_mut(&window_of(key))
                    && grant.is_some_and(|g| g.qp == qp && g.key == key)
                {
                    *grant = None;
                    return 0;
                }
                wc::REM_ACCESS_ERR
            }
            Action::Write {
                region,
                address,
                key,
                length,
            } => {
                let Some(data) = self.read(region, 0, length) else {
                    return wc::LOC_PROT_ERR;
                };
                self.remote(qp, |world| world.place(qp, &data, address, key))
            }
            Action::WriteImm {
                region,
                offset,
                length,
                address,
                key,
                imm,
            } => {
                let Some(data) = self.read(region, offset, length) else {
                    return wc::LOC_PROT_ERR;
                };
                self.remote(qp, |world| {
                    world.place(qp, &data, address, key)?;
                    match world.deliver(qp, work, Some(imm), None, length) {
                        0 => Ok(()),
                        status => Err(status),
                    }
                })
            }
            Action::Send {
                region,
                offset,
                length,
            } => {
                let Some(data) = self.read(region, offset, length) else {
                    return wc::LOC_PROT_ERR;
                };
                self.remote(qp, |world| {
                    match world.deliver(qp, work, None, Some(&data), length) {
                        0 => Ok(()),
                        status => Err(status),
                    }
                })
            }
        }
    }

    /// Run a remote operation; on failure move the peer to the error state.
    fn remote(
        &mut self,
        qp: u32,
        run: impl FnOnce(&mut Self) -> std::result::Result<(), u32>,
    ) -> u32 {
        match run(self) {
            Ok(()) => 0,
            Err(status) => {
                if let Some(peer) = self.peer(qp) {
                    self.set_error(peer);
                }
                status
            }
        }
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
unsafe extern "C" fn qp(
    device: *mut c_void,
    port: u8,
    _: u32,
    _: u32,
    qpn: *mut u32,
) -> *mut c_void {
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
            error: false,
            work: VecDeque::new(),
            recvs: VecDeque::new(),
            cq: VecDeque::new(),
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

/// Stop (destroy) a QP: drop its queued work, receives, and completions,
/// and unbind its windows. No CQEs are produced.
unsafe extern "C" fn stop(raw: *mut c_void) -> c_int {
    let Some(h) = (unsafe { handle(raw) }) else {
        return -1;
    };
    let mut world = h.sim.world.borrow_mut();
    world.call(Operation::Stop, h.id, |world| {
        let Some(Resource::Qp {
            stopped,
            work,
            recvs,
            cq,
            ..
        }) = world.resources.get_mut(&h.id)
        else {
            return -1;
        };
        *stopped = true;
        work.clear();
        recvs.clear();
        cq.clear();
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
                Resource::Qp {
                    device,
                    work,
                    recvs,
                    ..
                } => {
                    *device == h.id
                        || work.iter().any(|w| w.action.uses(h.id))
                        || recvs.iter().any(|r| r.region == h.id)
                }
                Resource::Region { device, .. } => *device == h.id,
                Resource::Window { device, grant, .. } => {
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
                Some(Resource::Qp { work, recvs, .. }) if !work.is_empty() || !recvs.is_empty()
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

/// Free a QP. Fails while it has work, receives, or bound windows.
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

/// Create a zeroed region with a fake wire address and `access` recorded.
unsafe extern "C" fn register(device: *mut c_void, length: u64, access: u32) -> *mut c_void {
    let Some(h) = (unsafe { handle(device) }) else {
        return std::ptr::null_mut();
    };
    let mut world = h.sim.world.borrow_mut();
    if length == 0
        || usize::try_from(length).is_err()
        || !matches!(access & !MR_REMOTE_WRITE, MR_PAGES | MR_CONTROL)
    {
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
            access,
        },
    );
    h.sim
        .world
        .borrow_mut()
        .record(Operation::Register, h.id, None, 0, false);
    raw
}

/// Register an implicit on-demand paging region, unless turned off with
/// [`Simulation::implicit_odp`]. Traced and faulted as `Register`.
unsafe extern "C" fn register_implicit(device: *mut c_void) -> *mut c_void {
    let Some(h) = (unsafe { handle(device) }) else {
        return std::ptr::null_mut();
    };
    let mut world = h.sim.world.borrow_mut();
    if world.no_implicit_odp || !matches!(world.resources.get(&h.id), Some(Resource::Device(_))) {
        world.record(Operation::Register, h.id, None, -1, false);
        return std::ptr::null_mut();
    }
    if world.rejected(Operation::Register, h.id) {
        return std::ptr::null_mut();
    }
    drop(world);
    let raw = allocate(
        &h.sim,
        Resource::Region {
            device: h.id,
            bytes: Box::default(),
            address: 0,
            access: MR_IMPLICIT,
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

/// Create an unbound window. Its key is `id << 8 | 1`.
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
            key: None,
            grant: None,
        },
    );
    unsafe {
        *key = (handle(raw).unwrap().id << 8) | 1;
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
    offset: u64,
    length: u32,
) -> c_int {
    let (Some(q), Some(w), Some(r)) = (unsafe { (handle(qp), handle(window), handle(region)) })
    else {
        return -1;
    };
    if !Rc::ptr_eq(&q.sim.world, &w.sim.world) || !Rc::ptr_eq(&q.sim.world, &r.sim.world) {
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
            key,
            offset,
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

/// The QP and region handles if both are non-null and share one fabric.
/// Traces a rejection for a foreign region.
unsafe fn qp_and_region<'a>(
    qp: *mut c_void,
    region: *mut c_void,
    operation: Operation,
    id: u64,
) -> Option<(&'a Handle, &'a Handle)> {
    let (Some(q), Some(r)) = (unsafe { (handle(qp), handle(region)) }) else {
        return None;
    };
    if !Rc::ptr_eq(&q.sim.world, &r.sim.world) {
        q.sim
            .world
            .borrow_mut()
            .record(operation, q.id, Some(id), -1, false);
        return None;
    }
    Some((q, r))
}

/// Queue a write with immediate. Checked when it completes.
#[allow(clippy::too_many_arguments)]
unsafe extern "C" fn write_imm(
    qp: *mut c_void,
    region: *mut c_void,
    offset: u64,
    length: u32,
    address: u64,
    key: u32,
    imm: u32,
    id: u64,
) -> c_int {
    let Some((q, r)) = (unsafe { qp_and_region(qp, region, Operation::WriteImm, id) }) else {
        return -1;
    };
    q.sim.world.borrow_mut().post(
        q.id,
        id,
        Action::WriteImm {
            region: r.id,
            offset,
            length,
            address,
            key,
            imm,
        },
    )
}

/// Queue a send. Delivered when it completes.
unsafe extern "C" fn send(
    qp: *mut c_void,
    region: *mut c_void,
    offset: u64,
    length: u32,
    id: u64,
) -> c_int {
    let Some((q, r)) = (unsafe { qp_and_region(qp, region, Operation::Send, id) }) else {
        return -1;
    };
    q.sim.world.borrow_mut().post(
        q.id,
        id,
        Action::Send {
            region: r.id,
            offset,
            length,
        },
    )
}

/// Post a receive.
unsafe extern "C" fn post_recv(
    qp: *mut c_void,
    region: *mut c_void,
    offset: u64,
    length: u32,
    id: u64,
) -> c_int {
    let Some((q, r)) = (unsafe { qp_and_region(qp, region, Operation::PostRecv, id) }) else {
        return -1;
    };
    q.sim.world.borrow_mut().post_recv(
        q.id,
        Recv {
            id,
            region: r.id,
            offset,
            length,
        },
    )
}

/// Report delivered receive completions, then run up to 32 queued requests
/// in order. Stops at a delay or a failure. In the error state, flushes.
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
            recvs,
            cq,
            error,
            stopped: false,
            ..
        }) = world.resources.get_mut(&q.id)
        else {
            break;
        };
        let (operation, completion, executed) = if let Some(c) = cq.pop_front() {
            (Operation::PostRecv, c, false)
        } else if *error {
            let (operation, id) = if let Some(w) = work.pop_front() {
                (w.action.operation(), w.id)
            } else if let Some(r) = recvs.pop_front() {
                (Operation::PostRecv, r.id)
            } else {
                break;
            };
            let flushed = Completion {
                id,
                status: wc::WR_FLUSH_ERR,
                opcode: u32::MAX,
                ..Completion::default()
            };
            (operation, flushed, false)
        } else {
            let Some(first) = work.front_mut() else {
                break;
            };
            if first.delay > 0 {
                first.delay -= 1;
                break;
            }
            if first.delivery == Some(Fault::Reorder) {
                if work.len() < 2 {
                    break;
                }
                work[0].delivery = None;
                work.swap(0, 1);
            }
            let pending = work.pop_front().unwrap();
            let status = if pending.status == 0 {
                world.execute(q.id, &pending)
            } else {
                pending.status
            };
            let completion = Completion {
                id: pending.id,
                status,
                opcode: if status == 0 {
                    pending.action.opcode()
                } else {
                    u32::MAX
                },
                ..Completion::default()
            };
            (pending.action.operation(), completion, true)
        };
        if completion.status != 0 {
            world.set_error(q.id);
        }
        world.record(
            operation,
            q.id,
            Some(completion.id),
            completion.status.into(),
            true,
        );
        unsafe {
            *out.add(count as usize) = completion;
        }
        count += 1;
        // After a failure, nothing more runs this poll. `ffi.rs` sees it and
        // stops the QP; a raw caller sees flushes on the next poll.
        if completion.status != 0 && executed {
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
            ..Completion::default()
        };
        unsafe {
            assert!(handle(raw).is_none());
            assert!(qp(raw, 1, 1, 1, &mut output).is_null());
            assert!(register(raw, 32, 5).is_null());
            assert!(window(raw, &mut output).is_null());
            assert!(bytes(raw).is_null());
            assert_eq!(connect(raw, &endpoint, &endpoint), -1);
            assert_eq!(stop(raw), -1);
            assert_eq!(bind(raw, raw, raw, 1, 2, 0, 3), -1);
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
                qp(device, 2, 1, 1, &mut output),
                register(device, 0, 5),
            ];
            sim.fault(Operation::Open, Fault::Reject);
            failures.push(open(c"sim0".as_ptr()));
            sim.fault(Operation::Qp, Fault::Reject);
            failures.push(qp(device, 1, 1, 1, &mut output));
            sim.fault(Operation::Register, Fault::Reject);
            failures.push(register(device, 32, 5));
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
                    assert_eq!(bind(q, w, r, window.key, 100, 0, 17), -1);
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
            assert!(qp(device, 2, 1, 1, &mut id).is_null());
            assert_eq!(id, 0);
            events(&sim, &[(Operation::Qp, d, None, -1, false)]);
            assert!(register(device, 0, 5).is_null());
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
                        assert!(qp(device, 1, 1, 1, &mut id).is_null());
                        d
                    }
                    Operation::Register => {
                        assert!(register(device, 32, 5).is_null());
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
            let q = qp(device, 1, 1, 1, &mut qpn);
            let local = Endpoint {
                gid: [1; 16],
                qpn,
                psn: 0,
                mtu: 3,
                lid: 0,
                port: 1,
                link_layer: 2,
            };
            let peer = qp(device, 1, 1, 1, &mut qpn);
            let remote = Endpoint { qpn, ..local };
            let region = register(device, 32, 5);
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
            assert_eq!(bind(q, w, region, key, 7, 0, 17), 0);
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
                    assert_eq!(bind(q, w.raw.as_ptr(), region, key, 41, 0, 17), -1);
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
                // The failed write moves the QP to the error state, so the
                // invalidate behind it is flushed rather than executed.
                for (operation, id, status) in [
                    (Operation::Write, 44, wc::REM_ACCESS_ERR),
                    (Operation::Invalidate, 45, wc::WR_FLUSH_ERR),
                ] {
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
                    assert_eq!(completion.status, status);
                    events(
                        &sim,
                        &[
                            (operation, qpn, Some(id), status.into(), true),
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

    /// Two connected QPs on one device with explicit depths, a page region,
    /// and a control region.
    struct Link {
        a: Rc<NativeQueuePair>,

        b: Rc<NativeQueuePair>,

        pages: Rc<NativeRegion>,

        control: Rc<NativeRegion>,
    }

    /// A caller-chosen work request id.
    const fn wr(n: u64) -> u64 {
        (1 << 56) | n
    }

    /// Build a [`Link`]. `before_connect` runs on `b` before either side
    /// connects.
    fn link(sim: &Simulation, before_connect: impl FnOnce(&Link)) -> Link {
        let node = sim
            .with_devices(vec![Device::new("sim0", [1; 16])])
            .unwrap();
        let _scope = node.enter();
        let device = Rc::new(super::super::discover().unwrap().remove(0));
        let qp = || {
            NativeQueuePair::new_with_depth(
                device.clone(),
                GuardOwner::new(Arc::new(())),
                Arc::new(std::sync::atomic::AtomicBool::new(false)),
                8,
                4,
            )
            .unwrap()
        };
        let link = Link {
            a: qp(),
            b: qp(),
            pages: NativeRegion::with_access(
                device.clone(),
                8192,
                Access::PAGES,
                lifetime_tests::quota().0,
            )
            .unwrap(),
            control: NativeRegion::with_access(
                device,
                1024,
                Access::CONTROL,
                lifetime_tests::quota().0,
            )
            .unwrap(),
        };
        before_connect(&link);
        link.a.connect(link.b.endpoint).unwrap();
        link.b.connect(link.a.endpoint).unwrap();
        link
    }

    /// Successful completions from one progress call.
    fn drain(qp: &NativeQueuePair) -> Result<Vec<WorkCompletion>> {
        let mut out = Vec::new();
        qp.progress_with(|c| out.push(c))?;
        Ok(out)
    }

    /// A completion record for comparisons.
    fn done(id: u64, opcode: u32, byte_len: u32, imm: Option<u32>) -> WorkCompletion {
        WorkCompletion {
            id,
            opcode,
            byte_len,
            imm,
        }
    }

    /// Read `length` bytes of a region at `offset`. The caller guarantees
    /// the NIC is done with the range.
    fn peek(region: &NativeRegion, offset: usize, length: usize) -> Vec<u8> {
        unsafe { std::slice::from_raw_parts(region.as_ptr().add(offset), length) }.to_vec()
    }

    /// Write `bytes` into a region at `offset`.
    fn poke(region: &NativeRegion, offset: usize, bytes: &[u8]) {
        unsafe {
            std::ptr::copy_nonoverlapping(bytes.as_ptr(), region.as_ptr().add(offset), bytes.len());
        }
    }

    /// Bind a fresh window on `b` over `pages[offset..offset + length]`.
    fn grant(link: &Link, id: u64, offset: u64, length: u32) -> (Rc<MemoryWindow>, u32) {
        let window = MemoryWindow::new(&link.b.device).unwrap();
        let key = link
            .b
            .bind_window(id, &window, &link.pages, offset, length)
            .unwrap();
        assert_eq!(
            drain(&link.b).unwrap(),
            [done(id, opcode::BIND_MW, 0, None)]
        );
        (window, key)
    }

    /// Stop both QPs so every region is idle.
    fn stop_both(link: &Link) {
        link.a.stop().unwrap();
        link.b.stop().unwrap();
    }

    #[test]
    /// A send lands in the peer's oldest receive, posted before connect.
    fn send_consumes_receives_in_order_and_reports_length() {
        let sim = Simulation::new();
        {
            let link = link(&sim, |l| {
                l.b.post_recv(wr(1), &l.control, 0, 64).unwrap();
                l.b.post_recv(wr(2), &l.control, 64, 64).unwrap();
            });
            poke(&link.control, 512, &[7; 64]);
            poke(&link.control, 576, &[9; 40]);
            link.a.send(wr(10), &link.control, 512, 64).unwrap();
            link.a.send(wr(11), &link.control, 576, 40).unwrap();
            assert_eq!(drain(&link.b).unwrap(), []);
            assert_eq!(
                drain(&link.a).unwrap(),
                [
                    done(wr(10), opcode::SEND, 0, None),
                    done(wr(11), opcode::SEND, 0, None)
                ]
            );
            assert_eq!(
                drain(&link.b).unwrap(),
                [
                    done(wr(1), opcode::RECV, 64, None),
                    done(wr(2), opcode::RECV, 40, None)
                ]
            );
            assert_eq!(peek(&link.control, 0, 64), [7; 64]);
            assert_eq!(peek(&link.control, 64, 40), [9; 40]);
            assert_eq!(peek(&link.control, 104, 24), [0; 24]);
            assert!(link.a.ready() && link.b.ready());
            stop_both(&link);
        }
        assert_eq!(sim.live_resources(), 0);
    }

    #[test]
    /// A write with immediate places data inside its grant and completes
    /// the peer's receive with the immediate; invalidation then revokes it.
    fn write_imm_places_data_and_reports_immediate_until_invalidated() {
        let sim = Simulation::new();
        {
            let link = link(&sim, |_| {});
            let (window, key) = grant(&link, wr(1), 4096, 100);
            link.b.post_recv(wr(2), &link.control, 0, 64).unwrap();
            poke(&link.pages, 0, &[0xa5; 100]);
            let address = link.pages.address() + 4096;
            link.a
                .write_imm(wr(3), &link.pages, 0, 100, address, key, 0xfeed)
                .unwrap();
            assert_eq!(
                drain(&link.a).unwrap(),
                [done(wr(3), opcode::RDMA_WRITE, 0, None)]
            );
            assert_eq!(
                drain(&link.b).unwrap(),
                [done(wr(2), opcode::RECV_RDMA_WITH_IMM, 100, Some(0xfeed))]
            );
            link.b.invalidate_window(wr(4), &window, key).unwrap();
            assert_eq!(
                drain(&link.b).unwrap(),
                [done(wr(4), opcode::LOCAL_INV, 0, None)]
            );
            assert_eq!(peek(&link.pages, 4096, 100), [0xa5; 100]);
            // The stale key is refused: remote access error, both sides break,
            // and the receiver's posted receive is flushed.
            link.b.post_recv(wr(5), &link.control, 0, 64).unwrap();
            poke(&link.pages, 0, &[0xff; 100]);
            link.a
                .write_imm(wr(6), &link.pages, 0, 100, address, key, 1)
                .unwrap();
            assert_eq!(drain(&link.a), Err(Error::Io));
            assert_eq!(link.a.failure().unwrap().status, status::REM_ACCESS_ERR);
            assert_eq!(drain(&link.b), Err(Error::Io));
            assert_eq!(
                link.b.failure().unwrap(),
                Failure {
                    id: wr(5),
                    status: status::WR_FLUSH_ERR,
                    vendor_err: 0
                }
            );
            assert_eq!(peek(&link.pages, 4096, 100), [0xa5; 100]);
            stop_both(&link);
        }
        assert_eq!(sim.live_resources(), 0);
    }

    #[test]
    /// Writes outside the bound range, on another window's key, or with an
    /// earlier generation of the same window are refused without writing.
    fn write_imm_respects_bound_range_and_key_generation() {
        for case in 0..5 {
            let sim = Simulation::new();
            {
                let link = link(&sim, |_| {});
                let (window, first) = grant(&link, wr(1), 4096, 100);
                link.b.invalidate_window(wr(2), &window, first).unwrap();
                drain(&link.b).unwrap();
                let second = link
                    .b
                    .bind_window(wr(3), &window, &link.pages, 4096, 100)
                    .unwrap();
                drain(&link.b).unwrap();
                assert_ne!(first, second);
                link.b.post_recv(wr(4), &link.control, 0, 64).unwrap();
                let base = link.pages.address() + 4096;
                let (address, length, key) = match case {
                    0 => (base - 1, 10, second),
                    1 => (base + 91, 10, second),
                    2 => (base, 10, first),
                    3 => (base, 10, second ^ 0x100),
                    _ => (base + 90, 10, second),
                };
                poke(&link.pages, 0, &[1; 10]);
                link.a
                    .write_imm(wr(5), &link.pages, 0, length, address, key, 0)
                    .unwrap();
                if case == 4 {
                    assert_eq!(drain(&link.a).unwrap().len(), 1);
                    assert_eq!(drain(&link.b).unwrap().len(), 1);
                    assert_eq!(peek(&link.pages, 4096 + 90, 10), [1; 10]);
                } else {
                    assert_eq!(drain(&link.a), Err(Error::Io));
                    assert_eq!(link.a.failure().unwrap().status, status::REM_ACCESS_ERR);
                    assert_eq!(drain(&link.b), Err(Error::Io));
                    assert_eq!(peek(&link.pages, 4095, 102), [0; 102]);
                }
                stop_both(&link);
            }
            assert_eq!(sim.live_resources(), 0);
        }
    }

    #[test]
    /// With rnr_retry = 0, a send or write with immediate and no posted
    /// receive fails with RNR and moves both QPs to the error state.
    fn missing_receive_is_rnr_and_breaks_both_sides() {
        for write in [false, true] {
            let sim = Simulation::new();
            {
                let link = link(&sim, |_| {});
                let (_window, key) = grant(&link, wr(1), 0, 64);
                poke(&link.pages, 4096, &[3; 64]);
                if write {
                    let address = link.pages.address();
                    link.a
                        .write_imm(wr(2), &link.pages, 4096, 64, address, key, 9)
                        .unwrap();
                } else {
                    link.a.send(wr(2), &link.pages, 4096, 64).unwrap();
                }
                assert_eq!(drain(&link.a), Err(Error::Io));
                assert_eq!(
                    link.a.failure(),
                    Some(Failure {
                        id: wr(2),
                        status: status::RNR_RETRY_EXC_ERR,
                        vendor_err: 0
                    })
                );
                // The receiver saw nothing, but is in the error state now: a
                // late receive is flushed.
                assert_eq!(drain(&link.b), Ok(vec![]));
                link.b.post_recv(wr(3), &link.control, 0, 64).unwrap();
                assert_eq!(drain(&link.b), Err(Error::Io));
                assert_eq!(link.b.failure().unwrap().status, status::WR_FLUSH_ERR);
                stop_both(&link);
                // A write with immediate may have placed data before the RNR.
                let expected = if write { [3; 64] } else { [0; 64] };
                assert_eq!(peek(&link.pages, 0, 64), expected);
            }
            assert_eq!(sim.live_resources(), 0);
        }
    }

    #[test]
    /// A send longer than the receive fails on both sides.
    fn send_longer_than_receive_is_a_length_error() {
        let sim = Simulation::new();
        {
            let link = link(&sim, |_| {});
            link.b.post_recv(wr(1), &link.control, 0, 32).unwrap();
            link.a.send(wr(2), &link.control, 512, 33).unwrap();
            assert_eq!(drain(&link.a), Err(Error::Io));
            assert_eq!(link.a.failure().unwrap().status, 9); // IBV_WC_REM_INV_REQ_ERR
            assert_eq!(drain(&link.b), Err(Error::Io));
            assert_eq!(link.b.failure().unwrap().status, 1); // IBV_WC_LOC_LEN_ERR
            stop_both(&link);
        }
        assert_eq!(sim.live_resources(), 0);
    }

    #[test]
    /// Writes, sends, and a fenced invalidate on one QP complete in post order.
    fn send_queue_work_completes_in_post_order() {
        let sim = Simulation::new();
        {
            let link = link(&sim, |_| {});
            let (window, key) = grant(&link, wr(1), 0, 64);
            for i in 0..3 {
                link.b
                    .post_recv(wr(10 + i), &link.control, i * 64, 64)
                    .unwrap();
            }
            let address = link.pages.address();
            link.a
                .write_imm(wr(20), &link.pages, 4096, 8, address, key, 1)
                .unwrap();
            link.a.send(wr(21), &link.control, 512, 8).unwrap();
            link.a
                .write_imm(wr(22), &link.pages, 4096, 8, address, key, 2)
                .unwrap();
            assert_eq!(
                drain(&link.a).unwrap(),
                [
                    done(wr(20), opcode::RDMA_WRITE, 0, None),
                    done(wr(21), opcode::SEND, 0, None),
                    done(wr(22), opcode::RDMA_WRITE, 0, None)
                ]
            );
            assert_eq!(
                drain(&link.b).unwrap(),
                [
                    done(wr(10), opcode::RECV_RDMA_WITH_IMM, 8, Some(1)),
                    done(wr(11), opcode::RECV, 8, None),
                    done(wr(12), opcode::RECV_RDMA_WITH_IMM, 8, Some(2))
                ]
            );
            // A fenced invalidate completes only after earlier work on its QP.
            link.a.post_recv(wr(31), &link.control, 832, 64).unwrap();
            sim.fault(Operation::Send, Fault::Delay(1));
            link.b.send(wr(30), &link.control, 768, 8).unwrap();
            link.b.invalidate_window(wr(32), &window, key).unwrap();
            assert_eq!(drain(&link.b).unwrap(), []);
            assert_eq!(
                drain(&link.b).unwrap(),
                [
                    done(wr(30), opcode::SEND, 0, None),
                    done(wr(32), opcode::LOCAL_INV, 0, None)
                ]
            );
            assert_eq!(
                drain(&link.a).unwrap(),
                [done(wr(31), opcode::RECV, 8, None)]
            );
            assert_eq!(link.a.failure(), None);
            stop_both(&link);
        }
        assert_eq!(sim.live_resources(), 0);
    }

    #[test]
    /// Drop, reorder, duplicate, and length corruption reach the receiver
    /// as the broken fabric would deliver them.
    fn delivery_faults_model_a_broken_fabric() {
        let sim = Simulation::new();
        {
            let link = link(&sim, |_| {});
            let (_window, key) = grant(&link, wr(1), 0, 64);
            let address = link.pages.address();
            for i in 0..4 {
                link.b
                    .post_recv(wr(10 + i), &link.control, i * 64, 64)
                    .unwrap();
            }
            // Drop: the sender succeeds, the receiver sees nothing.
            sim.fault(Operation::Send, Fault::Drop);
            link.a.send(wr(20), &link.control, 512, 8).unwrap();
            assert_eq!(drain(&link.a).unwrap().len(), 1);
            assert_eq!(drain(&link.b).unwrap(), []);
            // Reorder: held until the next request, then delivered after it.
            sim.fault(Operation::WriteImm, Fault::Reorder);
            link.a
                .write_imm(wr(21), &link.pages, 4096, 8, address, key, 1)
                .unwrap();
            assert_eq!(drain(&link.a).unwrap(), []);
            link.a.send(wr(22), &link.control, 512, 8).unwrap();
            assert_eq!(
                drain(&link.a).unwrap(),
                [
                    done(wr(22), opcode::SEND, 0, None),
                    done(wr(21), opcode::RDMA_WRITE, 0, None)
                ]
            );
            assert_eq!(
                drain(&link.b).unwrap(),
                [
                    done(wr(10), opcode::RECV, 8, None),
                    done(wr(11), opcode::RECV_RDMA_WITH_IMM, 8, Some(1))
                ]
            );
            // Duplicate: two receive completions with the same immediate.
            sim.fault(Operation::WriteImm, Fault::DuplicateImmediate);
            link.a
                .write_imm(wr(23), &link.pages, 4096, 8, address, key, 2)
                .unwrap();
            drain(&link.a).unwrap();
            assert_eq!(
                drain(&link.b).unwrap(),
                [
                    done(wr(12), opcode::RECV_RDMA_WITH_IMM, 8, Some(2)),
                    done(wr(13), opcode::RECV_RDMA_WITH_IMM, 8, Some(2))
                ]
            );
            // Corrupt length: a RECV longer than its buffer breaks the QP.
            link.b.post_recv(wr(14), &link.control, 0, 64).unwrap();
            sim.fault(Operation::Send, Fault::ByteLength(65));
            link.a.send(wr(24), &link.control, 512, 8).unwrap();
            drain(&link.a).unwrap();
            assert_eq!(drain(&link.b), Err(Error::Io));
            assert_eq!(
                link.b.failure(),
                Some(Failure {
                    id: wr(14),
                    status: 0,
                    vendor_err: 0
                })
            );
            stop_both(&link);
        }
        assert_eq!(sim.live_resources(), 0);
    }

    #[test]
    /// Scripted call and completion failures on the new operations.
    fn scripted_failures_on_send_receive_and_write_imm() {
        let sim = Simulation::new();
        {
            let link = link(&sim, |_| {});
            for operation in [Operation::Send, Operation::WriteImm, Operation::PostRecv] {
                sim.fault(operation, Fault::Reject);
                let result = match operation {
                    Operation::Send => link.a.send(wr(1), &link.control, 0, 8),
                    Operation::WriteImm => {
                        link.a
                            .write_imm(wr(1), &link.pages, 0, 8, link.pages.address(), 1, 0)
                    }
                    _ => link.a.post_recv(wr(1), &link.control, 0, 8),
                };
                assert_eq!(result, Err(Error::Io));
                assert!(link.a.ready());
            }
            assert_eq!(sim.pending_faults(), 0);
            link.b.post_recv(wr(2), &link.control, 0, 64).unwrap();
            sim.fault_on(
                Operation::Send,
                Some(link.a.endpoint.qpn),
                Fault::Completion(status::RNR_RETRY_EXC_ERR),
            );
            link.a.send(wr(3), &link.control, 512, 8).unwrap();
            link.a.send(wr(4), &link.control, 512, 8).unwrap();
            assert_eq!(drain(&link.a), Err(Error::Io));
            assert_eq!(link.a.failure().unwrap().status, status::RNR_RETRY_EXC_ERR);
            // The injected failure skipped delivery.
            assert_eq!(drain(&link.b), Ok(vec![]));
            stop_both(&link);
        }
        assert_eq!(sim.live_resources(), 0);
    }

    #[test]
    #[should_panic]
    /// Delivery faults only apply to operations that deliver to a peer.
    fn delivery_fault_on_bind_is_a_test_bug() {
        Simulation::new().fault(Operation::Bind, Fault::Drop);
    }

    #[test]
    #[should_panic]
    /// Receives have no completion of their own to delay.
    fn delay_on_post_recv_is_a_test_bug() {
        Simulation::new().fault(Operation::PostRecv, Fault::Delay(1));
    }

    #[test]
    /// Regions record access; binds need MW_BIND, and remote write on the
    /// region when the provider option requires it.
    fn region_access_controls_window_binds() {
        for require in [false, true] {
            let sim = Simulation::new();
            sim.require_mr_remote_write(require);
            {
                let link = link(&sim, |_| {});
                let remote = NativeRegion::with_access(
                    link.b.device.clone(),
                    64,
                    Access::PAGES_REMOTE_WRITE,
                    lifetime_tests::quota().0,
                )
                .unwrap();
                let window = MemoryWindow::new(&link.b.device).unwrap();
                link.b.bind_window(wr(1), &window, &remote, 0, 64).unwrap();
                assert_eq!(drain(&link.b).unwrap().len(), 1);
                // The ranged API refuses control regions before posting.
                let other = MemoryWindow::new(&link.b.device).unwrap();
                assert_eq!(
                    link.b.bind_window(wr(2), &other, &link.control, 0, 64),
                    Err(Error::InvalidRequest)
                );
                link.b
                    .bind_window(wr(3), &other, &link.pages, 0, 64)
                    .unwrap();
                if require {
                    assert_eq!(drain(&link.b), Err(Error::Io));
                    assert_eq!(link.b.failure().unwrap().status, 6); // IBV_WC_MW_BIND_ERR
                } else {
                    assert_eq!(drain(&link.b).unwrap().len(), 1);
                }
                stop_both(&link);
            }
            assert_eq!(sim.live_resources(), 0);
        }
    }

    #[test]
    /// Raw adapter contracts: access validation, binds on control regions,
    /// flush records, and receives that pin their region and QP.
    fn raw_receive_queue_and_access_contracts() {
        let sim = Simulation::new()
            .with_devices(vec![Device::new("sim0", [1; 16])])
            .unwrap();
        let _scope = sim.enter();
        unsafe {
            let device = open(c"sim0".as_ptr());
            for access in [0, 3, 4, 8, 7] {
                assert!(register(device, 32, access).is_null());
            }
            let control = register(device, 64, 2);
            let pages = register(device, 64, 6);
            assert!(!control.is_null() && !pages.is_null());
            let mut qpn = 0;
            let a = qp(device, 1, 4, 4, &mut qpn);
            let local = Endpoint {
                gid: [1; 16],
                qpn,
                psn: 0,
                mtu: 3,
                lid: 0,
                port: 1,
                link_layer: 2,
            };
            let b = qp(device, 1, 4, 4, &mut qpn);
            let remote = Endpoint { qpn, ..local };
            // Receives work before connect; sends do not.
            assert_eq!(post_recv(a, control, 0, 64, 1), 0);
            assert_eq!(post_recv(a, control, 1, 64, 2), -1);
            assert_eq!(post_recv(a, control, 0, 0, 2), -1);
            assert_eq!(send(a, control, 0, 8, 3), -1);
            assert_eq!(connect(a, &local, &remote), 0);
            assert_eq!(connect(b, &remote, &local), 0);
            // A bind on a control region fails on completion.
            let mut key = 0;
            let w = window(device, &mut key);
            assert_eq!(key & 0xff, 1);
            // Wrong window index, then the right one.
            assert_eq!(bind(a, w, control, key + 0x100, 4, 0, 8), -1);
            assert_eq!(bind(a, w, control, key, 4, 0, 8), 0);
            let mut c = Completion::default();
            assert_eq!(poll(a, &mut c, 1), 1);
            assert_eq!((c.id, c.status, c.opcode), (4, 6, u32::MAX));
            // In the error state, posted work and receives flush.
            assert_eq!(send(a, control, 0, 8, 5), 0);
            let mut out = [Completion::default(); 4];
            assert_eq!(poll(a, out.as_mut_ptr(), 4), 2);
            assert_eq!(
                out[..2]
                    .iter()
                    .map(|c| (c.id, c.status, c.opcode))
                    .collect::<Vec<_>>(),
                [(5, 5, u32::MAX), (1, 5, u32::MAX)]
            );
            // A posted receive pins its region and QP until stop.
            assert_eq!(post_recv(b, control, 0, 64, 6), 0);
            assert_eq!(deregister(control), -1);
            assert_eq!(qp_free(b), -1);
            // A bind must use a new key generation.
            assert_eq!(bind(a, w, pages, key, 8, 0, 8), -1);
            assert_eq!(stop(b), 0);
            assert_eq!(stop(a), 0);
            assert_eq!(bind(b, w, pages, key, 7, 0, 8), -1);
            assert_eq!(window_free(w), 0);
            for (raw, operation) in [
                (control, Operation::Deregister),
                (pages, Operation::Deregister),
                (a, Operation::QpFree),
                (b, Operation::QpFree),
                (device, Operation::Close),
            ] {
                assert_eq!(free(raw, operation), 0);
            }
        }
        assert_eq!(sim.live_resources(), 0);
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
}
