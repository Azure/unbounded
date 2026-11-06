//! A fake RDMA fabric for tests. No hardware or sysfs needed.
//!
//! It implements the same C functions as `native/verbs.c`, so the real
//! wrappers in `ffi.rs` (and the registration-mode logic and connection
//! manager state machine in `registration.rs`) run on top of it unchanged.
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
//!    fabric for `Send` and `WriteImm`. Connection faults
//!    ([`Fault::AddressError`], [`Fault::RouteError`], [`Fault::Timeout`])
//!    model a broken connection manager.
//! 5. Check what happened with [`Simulation::trace`].
//!
//! # Device profiles
//!
//! Each [`Device`] has a [`Profile`]: [`Profile::Window`] (mlx5-like: memory
//! windows, implicit on-demand paging, plain verbs connect) or
//! [`Profile::Registration`] (MANA-like: no windows, no ODP, QPs connected
//! through the connection manager, addressed by the IPv4 address in the last
//! four bytes of the device GID). [`Device::new`] uses the thread's default
//! profile, set with [`default_profile`], so one test body can run against
//! both.
//!
//! Addresses seen on the wire are fake; they are never host pointers.
//! A dma-buf region maps the given descriptor, so a memfd from
//! [`MemfdMemory`] stands in for GPU memory. Its wire address is the iova.
//! Like the C adapter, sends, writes, and receives that name a dma-buf region
//! are rejected, and [`Operation::RegisterDmabuf`] traces and faults its
//! registration.
//! Device names must be unique within a discovery view, even for different ports.
//! A remote QP must already exist in this fabric before its peer connects to it.
//!
//! # Verbs model
//!
//! - Send work runs in post order when the posting QP's CQ is polled.
//!   Receive completions land on the peer's CQ and are reported by the
//!   peer's poll before its own send work.
//! - `Send` and `WriteImm` consume the peer's oldest posted receive. With none
//!   posted the sender fails with `IBV_WC_RNR_RETRY_EXC_ERR` (`rnr_retry` is
//!   0). A `WriteImm` whose key check passed has already placed its data;
//!   real multi-packet writes can do the same before the RNR. An unsignaled
//!   write reports nothing unless it fails.
//! - A remote access error, RNR, or receive length error moves both QPs to the
//!   error state. Any failed completion moves its own QP there. A QP in the
//!   error state flushes every posted work request with `IBV_WC_WR_FLUSH_ERR`.
//! - Until a QP is destroyed, RDMA writes through its live grants still land,
//!   even in the error state, so tests stay conservative about late DMA.
//! - Remote keys are `resource << 8 | generation`: window keys like
//!   `ibv_inc_rkey` (a bind must use a key with the window's index that
//!   differs from the last one), registration keys with generation 0.
//! - A CQ armed with `req_notify` raises one event on its completion channel
//!   (a real eventfd) when work is posted to its QP, a receive completion
//!   lands on it, its QP enters the error state, or a poll leaves work
//!   queued.
//! - The connection manager delivers events through a real eventfd per
//!   channel. In registration mode a QP is named by the token the caller
//!   passed in `*qpn` (trace resources and fault targets use it).
use super::*;

use std::os::fd::{AsRawFd, FromRawFd, OwnedFd};

thread_local! {
    static CURRENT: RefCell<Option<Simulation>> = const { RefCell::new(None) };

    static PROFILE: Cell<Profile> = const { Cell::new(Profile::Window) };
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

/// How a simulated device behaves.
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub enum Profile {
    /// Memory windows, implicit on-demand paging, plain verbs connect (mlx5).
    #[default]
    Window,

    /// No memory windows, no on-demand paging, connection manager connect
    /// (MANA).
    Registration,
}

/// Restores the previous default profile on drop. From [`default_profile`].
pub struct ProfileGuard {
    previous: Profile,
}

impl Drop for ProfileGuard {
    fn drop(&mut self) {
        PROFILE.with(|p| p.set(self.previous));
    }
}

/// Make `profile` the default for [`Device::new`] on this thread until the
/// guard drops.
pub fn default_profile(profile: Profile) -> ProfileGuard {
    ProfileGuard {
        previous: PROFILE.with(|p| p.replace(profile)),
    }
}

/// The thread's default profile.
pub fn current_profile() -> Profile {
    PROFILE.with(Cell::get)
}

/// A fake port that discovery will report.
#[derive(Clone, Debug)]
pub struct Device {
    pub name: String,

    pub gid: [u8; 16],

    pub port: u8,

    pub numa_node: Option<usize>,

    pub profile: Profile,
}

impl Device {
    /// Port 1 of `name`, with no NUMA node, in the thread's default profile.
    pub fn new(name: impl Into<String>, gid: [u8; 16]) -> Self {
        Self {
            name: name.into(),
            gid,
            port: 1,
            numa_node: None,
            profile: current_profile(),
        }
    }

    /// The IPv4 address registration mode uses: the GID's last four bytes.
    fn ipv4(&self) -> [u8; 4] {
        self.gid[12..].try_into().unwrap()
    }

    /// The IPv4-mapped GID of [`Self::ipv4`].
    fn ipv4_gid(&self) -> [u8; 16] {
        let mut gid = [0; 16];
        gid[10] = 0xff;
        gid[11] = 0xff;
        gid[12..].copy_from_slice(&self.ipv4());
        gid
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

    /// Create a CQ.
    Cq,

    /// Create a window-mode QP.
    Qp,

    Connect,

    /// Destroy a QP: the terminal fence.
    Stop,

    /// Free a QP's CQ, the last step of freeing a QP.
    QpFree,

    Register,

    RegisterDmabuf,

    Deregister,

    /// Register caller memory (registration mode grants and write sources).
    RegMr,

    /// Deregister caller memory.
    DeregMr,

    Window,

    WindowFree,

    Bind,

    Invalidate,

    /// A write with immediate.
    WriteImm,

    /// A write without immediate (a registration-mode write piece).
    Write,

    Send,

    PostRecv,

    Poll,

    CmListen,

    CmResolveAddr,

    CmResolveRoute,

    CmCreateQp,

    CmConnect,

    CmAccept,

    CmReject,

    CmDestroyId,
}

/// A one-shot failure for the next matching call.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Fault {
    /// Fail the call itself. Nothing changes.
    Reject,

    /// Hold a send-queue request for this many polls.
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

    /// `CmResolveAddr`: succeed, then report `ADDR_ERROR`.
    AddressError,

    /// `CmResolveRoute`: succeed, then report `ROUTE_ERROR`.
    RouteError,

    /// `CmConnect`: succeed, then never answer, like a lost peer.
    Timeout,
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

    /// Finished background registrations stay pending.
    hold_registrations: bool,

    /// Registration-profile maximum message size, if not the default.
    max_message: Option<u32>,

    /// `(max_qp_wr, max_cqe, max_mw)` every device reports; zero is unknown.
    device_limits: (u32, u32, u32),

    trace: Vec<Event>,

    sequence: u64,
}

/// A live simulated object.
enum Resource {
    Device(Device),

    /// A completion channel: CQ handles with events, signaled on `fd`.
    CompChannel {
        device: u32,

        fd: OwnedFd,

        events: VecDeque<usize>,
    },

    Cq {
        device: u32,

        channel: Option<u32>,

        armed: bool,

        /// This CQ's handle, reported in channel events.
        raw: usize,
    },

    Qp {
        device: u32,

        cq: u32,

        /// The name traces and faults use: the resource id, or the caller's
        /// token for a connection manager QP.
        alias: u32,

        local: Option<Endpoint>,

        remote: Option<Endpoint>,

        /// In the error state: posted work flushes.
        error: bool,

        work: VecDeque<Work>,

        recvs: VecDeque<Recv>,

        /// Delivered receive completions not yet polled.
        cq_entries: VecDeque<Completion>,
    },

    Region {
        device: u32,

        bytes: Memory,

        address: u64,

        /// `RDMA_VERBS_MR_*` bits.
        access: u32,
    },

    /// Caller memory registered with `reg_mr`.
    Mr {
        device: u32,

        /// Host address.
        addr: u64,

        length: u64,

        /// Wire address of `addr`: inside a region, the region's address.
        iova: u64,

        access: u32,

        /// The region holding the memory and the offset in it, if any.
        region: Option<(u32, u64)>,
    },

    Window {
        device: u32,

        /// Last key used for a bind, if any.
        key: Option<u32>,

        grant: Option<Grant>,
    },

    /// A connection manager event channel signaled on `fd`.
    CmChannel {
        device: u32,

        fd: OwnedFd,

        events: VecDeque<CmEvent>,
    },

    CmId {
        channel: u32,

        /// This id's handle, reported in events.
        raw: usize,

        /// Bound device: a listener's, or the resolved source's.
        device: Option<u32>,

        listen: Option<([u8; 4], u16)>,

        destination: Option<([u8; 4], u16)>,

        qp: Option<u32>,

        /// The other side of a connection.
        peer: Option<u32>,

        /// Events delivered and not yet acknowledged.
        unacked: u32,

        established: bool,
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

/// A posted receive into host memory named by `lkey`.
#[derive(Clone, Copy)]
struct Recv {
    id: u64,

    sge: Sge,
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

/// The resource a key names.
fn resource_of(key: u32) -> u32 {
    key >> 8
}

/// `RDMA_VERBS_MR_PAGES`.
const MR_PAGES: u32 = 1;

/// `RDMA_VERBS_MR_CONTROL`.
const MR_CONTROL: u32 = 2;

/// `RDMA_VERBS_MR_REMOTE_WRITE`.
const MR_REMOTE_WRITE: u32 = 4;

/// A dma-buf region from `rdma_verbs_register_dmabuf`. Peers write through
/// windows; never a send, write, or receive buffer.
const MR_DEVICE: u32 = 16;

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
#[derive(Clone)]
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
        sges: Vec<Sge>,

        address: u64,

        key: u32,

        imm: Option<u32>,

        signaled: bool,
    },

    Send {
        sges: Vec<Sge>,
    },
}

impl Action {
    /// The matching `Operation`.
    fn operation(&self) -> Operation {
        match self {
            Self::Bind { .. } => Operation::Bind,
            Self::Invalidate { .. } => Operation::Invalidate,
            Self::Write { imm: Some(_), .. } => Operation::WriteImm,
            Self::Write { imm: None, .. } => Operation::Write,
            Self::Send { .. } => Operation::Send,
        }
    }

    /// The completion opcode `ffi.rs` expects.
    fn opcode(&self) -> u32 {
        match self {
            Self::Bind { .. } => 5,
            Self::Invalidate { .. } => 6,
            Self::Write { .. } => 1,
            Self::Send { .. } => 0,
        }
    }

    /// True if a successful completion is reported.
    fn signaled(&self) -> bool {
        !matches!(
            self,
            Self::Write {
                signaled: false,
                ..
            }
        )
    }

    /// True if `id` is a region, registration, or window this work still uses.
    fn uses(&self, id: u32) -> bool {
        match self {
            Self::Bind { window, region, .. } => *window == id || *region == id,
            Self::Invalidate { key } => resource_of(*key) == id,
            Self::Write { sges, .. } | Self::Send { sges } => {
                sges.iter().any(|s| resource_of(s.lkey) == id)
            }
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
    /// faults for anything but `Send` and `WriteImm`, connection faults on
    /// other calls, or `Completion(0)`.
    pub fn fault_on(&self, operation: Operation, qpn: Option<u32>, fault: Fault) {
        let send_queue = matches!(
            operation,
            Operation::Bind
                | Operation::Invalidate
                | Operation::WriteImm
                | Operation::Write
                | Operation::Send
        );
        let delivers = matches!(operation, Operation::WriteImm | Operation::Send);
        assert!(match fault {
            Fault::Reject => true,
            Fault::Delay(_) | Fault::Completion(_) => send_queue,
            Fault::Drop | Fault::Reorder | Fault::DuplicateImmediate | Fault::ByteLength(_) =>
                delivers,
            Fault::AddressError => operation == Operation::CmResolveAddr,
            Fault::RouteError => operation == Operation::CmResolveRoute,
            Fault::Timeout => operation == Operation::CmConnect,
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

    /// Choose whether window-profile devices support implicit on-demand
    /// paging, the zero-copy write source. On by default.
    pub fn implicit_odp(&self, enabled: bool) {
        self.world.borrow_mut().no_implicit_odp = !enabled;
    }

    /// While `held`, finished background region registrations (page pools
    /// and dma-buf device pools) stay pending, as a slow `ibv_reg_mr` would.
    /// The owner keeps serving meanwhile.
    pub fn hold_registrations(&self, held: bool) {
        self.world.borrow_mut().hold_registrations = held;
    }

    /// Set the maximum message size registration-profile devices report.
    /// Below 8 MiB such a device is unusable.
    pub fn max_message(&self, bytes: u32) {
        self.world.borrow_mut().max_message = Some(bytes);
    }

    /// Set the `max_qp_wr`, `max_cqe`, and `max_mw` every device reports.
    /// Zero, the default, means unknown and unbounded.
    pub fn device_limits(&self, max_qp_wr: u32, max_cqe: u32, max_mw: u32) {
        self.world.borrow_mut().device_limits = (max_qp_wr, max_cqe, max_mw);
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

    /// Registrations from `reg_mr` not yet deregistered.
    pub fn live_registrations(&self) -> usize {
        self.world
            .borrow()
            .resources
            .values()
            .filter(|r| matches!(r, Resource::Mr { .. }))
            .count()
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
        open as unsafe extern "C" fn(*const c_char, c_int) -> *mut c_void,
    )
}

/// The function table `ffi.rs` uses in place of the C library.
pub(super) fn api() -> Rc<Api> {
    Rc::new(Api {
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
        prefetch,
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
    })
}

/// The opaque pointer handed to `ffi.rs`. Holds the fabric and a resource id.
/// The fabric owns the resource; `ffi.rs` decides when to free it.
struct Handle {
    sim: Simulation,

    id: u32,
}

/// True while [`Simulation::hold_registrations`] holds `device`'s world.
pub(crate) fn registrations_held(device: *mut c_void) -> bool {
    unsafe { handle(device) }.is_some_and(|h| h.sim.world.borrow().hold_registrations)
}

/// Return no handle for null. The caller keeps any non-null handle alive.
unsafe fn handle<'a>(raw: *mut c_void) -> Option<&'a Handle> {
    unsafe { raw.cast::<Handle>().as_ref() }
}

/// A new handle to resource `id`.
fn boxed(sim: &Simulation, id: u32) -> *mut c_void {
    Box::into_raw(Box::new(Handle {
        sim: sim.clone(),
        id,
    }))
    .cast()
}

/// Add a resource and return a new handle to it.
fn allocate(sim: &Simulation, resource: Resource) -> *mut c_void {
    let id = sim.world.borrow_mut().insert(resource);
    boxed(sim, id)
}

/// A nonblocking semaphore eventfd.
fn eventfd() -> OwnedFd {
    let fd = unsafe {
        libc::eventfd(
            0,
            libc::EFD_NONBLOCK | libc::EFD_CLOEXEC | libc::EFD_SEMAPHORE,
        )
    };
    assert!(fd >= 0, "simulation eventfd");
    unsafe { OwnedFd::from_raw_fd(fd) }
}

/// Add one to an eventfd.
fn signal(fd: &OwnedFd) {
    let one = 1u64;
    let written = unsafe { libc::write(fd.as_raw_fd(), (&raw const one).cast(), 8) };
    assert_eq!(written, 8, "simulation eventfd write");
}

/// Take one from an eventfd.
fn consume(fd: &OwnedFd) {
    let mut value = 0u64;
    let _ = unsafe { libc::read(fd.as_raw_fd(), (&raw mut value).cast(), 8) };
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

    /// The name of a resource in traces and fault targets.
    fn name(&self, id: u32) -> u32 {
        match self.resources.get(&id) {
            Some(Resource::Qp { alias, .. }) => *alias,
            _ => id,
        }
    }

    /// Add an event to the trace. `resource` is a resource id.
    fn record(
        &mut self,
        operation: Operation,
        resource: u32,
        work_id: Option<u64>,
        result: i64,
        completion: bool,
    ) {
        let resource = self.name(resource);
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
        let resource = self.name(resource);
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
            | Resource::Cq { device, .. }
            | Resource::CompChannel { device, .. }
            | Resource::CmChannel { device, .. }
            | Resource::Region { device, .. }
            | Resource::Mr { device, .. }
            | Resource::Window { device, .. } => Some(*device),
            Resource::CmId { device, .. } => *device,
        }
    }

    /// The device record of a device id.
    fn device_info(&self, id: u32) -> Option<&Device> {
        match self.resources.get(&id)? {
            Resource::Device(d) => Some(d),
            _ => None,
        }
    }

    /// True if a QP is connected.
    fn ready(&self, id: u32) -> bool {
        matches!(
            self.resources.get(&id),
            Some(Resource::Qp {
                local: Some(_),
                remote: Some(_),
                ..
            })
        )
    }

    /// True if two QPs are connected to each other.
    fn paired(&self, id: u32, peer: u32) -> bool {
        match (self.resources.get(&id), self.resources.get(&peer)) {
            (
                Some(Resource::Qp {
                    local: Some(a),
                    remote: Some(b),
                    ..
                }),
                Some(Resource::Qp {
                    local: Some(c),
                    remote: Some(d),
                    ..
                }),
            ) => a == d && b == c,
            _ => false,
        }
    }

    /// Raise a completion channel event for `qp`'s CQ if it is armed.
    fn notify(&mut self, qp: u32) {
        let Some(Resource::Qp { cq, .. }) = self.resources.get(&qp) else {
            return;
        };
        let cq = *cq;
        let Some(Resource::Cq {
            armed,
            channel: Some(channel),
            raw,
            ..
        }) = self.resources.get_mut(&cq)
        else {
            return;
        };
        if !*armed {
            return;
        }
        *armed = false;
        let (channel, raw) = (*channel, *raw);
        if let Some(Resource::CompChannel { fd, events, .. }) = self.resources.get_mut(&channel) {
            events.push_back(raw);
            signal(fd);
        }
    }

    /// The host bytes `[sge.addr, sge.addr + sge.length)` in a region or
    /// registration named by `sge.lkey`, as `(region, offset)`, or `None`
    /// for memory outside any region (implicit or caller memory).
    /// `Err` if the key or range is invalid.
    fn locate(
        &self,
        sge: &Sge,
        device: Option<u32>,
    ) -> std::result::Result<Option<(u32, u64)>, ()> {
        let id = resource_of(sge.lkey);
        if sge.length == 0 || sge.lkey & 0xff != 0 || self.device(id) != device {
            return Err(());
        }
        let end = sge.addr.checked_add(sge.length.into()).ok_or(())?;
        match self.resources.get(&id) {
            Some(Resource::Region { access, .. }) if access & MR_IMPLICIT != 0 => {
                usize::try_from(end).map_err(|_| ())?;
                Ok(None)
            }
            Some(Resource::Region {
                access,
                bytes: Memory::Host(bytes),
                ..
            }) if access & MR_DEVICE == 0 => {
                let base = bytes.as_ptr() as u64;
                let offset = sge.addr.checked_sub(base).ok_or(())?;
                if end - base > bytes.len() as u64 {
                    return Err(());
                }
                Ok(Some((id, offset)))
            }
            Some(Resource::Mr {
                addr,
                length,
                region,
                ..
            }) => {
                let delta = sge.addr.checked_sub(*addr).ok_or(())?;
                if delta + u64::from(sge.length) > *length {
                    return Err(());
                }
                Ok(region.map(|(r, offset)| (r, offset + delta)))
            }
            _ => Err(()),
        }
    }

    /// A copy of the gathered bytes.
    fn gather(&self, sges: &[Sge], device: Option<u32>) -> Option<Vec<u8>> {
        let mut out = Vec::new();
        for sge in sges {
            match self.locate(sge, device).ok()? {
                Some((region, offset)) => {
                    let Some(Resource::Region { bytes, .. }) = self.resources.get(&region) else {
                        return None;
                    };
                    out.extend_from_slice(
                        &bytes[offset as usize..offset as usize + sge.length as usize],
                    );
                }
                // SAFETY: caller memory named by an implicit region or a
                // registration. The verbs owner holds the source until this
                // work completes or the QP is destroyed, as the NIC requires.
                None => out.extend_from_slice(unsafe {
                    std::slice::from_raw_parts(sge.addr as usize as *const u8, sge.length as usize)
                }),
            }
        }
        Some(out)
    }

    /// Check and queue one send-queue request. Returns -1 if rejected.
    fn post(&mut self, qp: u32, id: u64, action: Action) -> c_int {
        let op = action.operation();
        let fault = self.fault(op, qp);
        let device = self.device(qp);
        let valid = self.ready(qp)
            && match &action {
                Action::Bind {
                    window,
                    region,
                    key,
                    offset,
                    length,
                } => {
                    self.device(*window) == device
                        && self.device(*region) == device
                        && resource_of(*key) == *window
                        && matches!(
                            self.resources.get(window),
                            Some(Resource::Window { grant: None, key: last, .. }) if *last != Some(*key)
                        )
                        && self.region_fits(*region, *offset, *length)
                }
                Action::Write { sges, .. } | Action::Send { sges } => {
                    !sges.is_empty()
                        && sges.len() <= 4
                        && sges.iter().all(|s| self.locate(s, device).is_ok())
                }
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
        self.notify(qp);
        0
    }

    /// Check and queue one receive. Allowed before connect, like verbs in
    /// the INIT state. Returns -1 if rejected.
    fn post_recv(&mut self, qp: u32, recv: Recv) -> c_int {
        let fault = self.fault(Operation::PostRecv, qp);
        let valid = matches!(self.resources.get(&qp), Some(Resource::Qp { .. }))
            && matches!(self.locate(&recv.sge, self.device(qp)), Ok(Some(_)));
        if !valid || fault == Some(Fault::Reject) {
            self.record(Operation::PostRecv, qp, Some(recv.id), -1, false);
            return -1;
        }
        if let Some(Resource::Qp { recvs, error, .. }) = self.resources.get_mut(&qp) {
            recvs.push_back(recv);
            if *error {
                self.notify(qp);
            }
        }
        self.record(Operation::PostRecv, qp, Some(recv.id), 0, false);
        0
    }

    /// True if `[offset, offset + length)` is a nonempty range of the region.
    fn region_fits(&self, region: u32, offset: u64, length: u32) -> bool {
        matches!(
            self.resources.get(&region),
            Some(Resource::Region { bytes, access, .. }) if access & MR_IMPLICIT == 0 && length > 0
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
            if *error {
                return;
            }
            *error = true;
        }
        self.notify(qp);
    }

    /// Place an RDMA write through `key` at the peer's `address`.
    /// Returns a verbs error status with nothing written.
    fn place(
        &mut self,
        qp: u32,
        data: &[u8],
        address: u64,
        key: u32,
    ) -> std::result::Result<(), u32> {
        let target = resource_of(key);
        let peer = self.peer(qp).filter(|peer| self.paired(qp, *peer));
        let (region, offset, length) = match self.resources.get(&target) {
            Some(Resource::Window {
                grant: Some(grant), ..
            }) => {
                let grant = *grant;
                if grant.key != key || !self.paired(qp, grant.qp) {
                    return Err(wc::REM_ACCESS_ERR);
                }
                let Some(Resource::Region { address: base, .. }) =
                    self.resources.get(&grant.region)
                else {
                    return Err(wc::REM_ACCESS_ERR);
                };
                let Some(offset) = address.checked_sub(*base) else {
                    return Err(wc::REM_ACCESS_ERR);
                };
                let end = offset.checked_add(data.len() as u64);
                if offset < grant.offset
                    || end.is_none_or(|end| end > grant.offset + u64::from(grant.length))
                {
                    return Err(wc::REM_ACCESS_ERR);
                }
                (Some(grant.region), offset, None)
            }
            Some(Resource::Mr {
                device,
                iova,
                length,
                access,
                region,
                addr,
            }) => {
                if key & 0xff != 0
                    || access & MR_REMOTE_WRITE == 0
                    || peer.and_then(|p| self.device(p)) != Some(*device)
                {
                    return Err(wc::REM_ACCESS_ERR);
                }
                let Some(delta) = address.checked_sub(*iova) else {
                    return Err(wc::REM_ACCESS_ERR);
                };
                if delta
                    .checked_add(data.len() as u64)
                    .is_none_or(|end| end > *length)
                {
                    return Err(wc::REM_ACCESS_ERR);
                }
                match region {
                    Some((r, offset)) => (Some(*r), offset + delta, None),
                    None => (None, 0, Some(addr + delta)),
                }
            }
            _ => return Err(wc::REM_ACCESS_ERR),
        };
        match (region, length) {
            (Some(region), _) => {
                let Some(Resource::Region { bytes, .. }) = self.resources.get_mut(&region) else {
                    return Err(wc::REM_ACCESS_ERR);
                };
                let Some(target) = bytes.get_mut(offset as usize..offset as usize + data.len())
                else {
                    return Err(wc::REM_ACCESS_ERR);
                };
                target.copy_from_slice(data);
            }
            // SAFETY: registered caller memory, kept alive while registered.
            (None, Some(host)) => unsafe {
                std::ptr::copy_nonoverlapping(data.as_ptr(), host as usize as *mut u8, data.len());
            },
            (None, None) => return Err(wc::REM_ACCESS_ERR),
        }
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
                let target = self.locate(&recv.sge, self.device(peer));
                if data.len() > recv.sge.length as usize {
                    completion = Completion {
                        id: recv.id,
                        status: wc::LOC_LEN_ERR,
                        opcode: u32::MAX,
                        ..Completion::default()
                    };
                    sender_status = wc::REM_INV_REQ_ERR;
                } else if let Ok(Some((region, offset))) = target
                    && let Some(Resource::Region { bytes, .. }) = self.resources.get_mut(&region)
                {
                    let start = offset as usize;
                    bytes[start..start + data.len()].copy_from_slice(data);
                } else {
                    completion.status = wc::LOC_PROT_ERR;
                    completion.opcode = u32::MAX;
                    sender_status = wc::REM_INV_REQ_ERR;
                }
            }
            if let Some(Resource::Qp { cq_entries, .. }) = self.resources.get_mut(&peer) {
                cq_entries.push_back(completion);
            }
            self.notify(peer);
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
        let device = self.device(qp);
        match &work.action {
            Action::Bind {
                window,
                region,
                key,
                offset,
                length,
            } => {
                let Some(Resource::Region { access, .. }) = self.resources.get(region) else {
                    return wc::MW_BIND_ERR;
                };
                if access & MR_PAGES == 0
                    || (self.require_mr_remote_write && access & MR_REMOTE_WRITE == 0)
                {
                    return wc::MW_BIND_ERR;
                }
                if let Some(Resource::Window { grant, .. }) = self.resources.get_mut(window)
                    && grant.is_none()
                {
                    *grant = Some(Grant {
                        qp,
                        region: *region,
                        key: *key,
                        offset: *offset,
                        length: *length,
                    });
                    return 0;
                }
                wc::MW_BIND_ERR
            }
            Action::Invalidate { key } => {
                if let Some(Resource::Window { grant, .. }) =
                    self.resources.get_mut(&resource_of(*key))
                    && grant.is_some_and(|g| g.qp == qp && g.key == *key)
                {
                    *grant = None;
                    return 0;
                }
                wc::REM_ACCESS_ERR
            }
            Action::Write {
                sges,
                address,
                key,
                imm,
                ..
            } => {
                let Some(data) = self.gather(sges, device) else {
                    return wc::LOC_PROT_ERR;
                };
                let (address, key, imm) = (*address, *key, *imm);
                self.remote(qp, |world| {
                    world.place(qp, &data, address, key)?;
                    if imm.is_none() {
                        return Ok(());
                    }
                    match world.deliver(qp, work, imm, None, data.len() as u32) {
                        0 => Ok(()),
                        status => Err(status),
                    }
                })
            }
            Action::Send { sges } => {
                let Some(data) = self.gather(sges, device) else {
                    return wc::LOC_PROT_ERR;
                };
                self.remote(qp, |world| {
                    match world.deliver(qp, work, None, Some(&data), data.len() as u32) {
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

    /// The QP using CQ `cq`, if any.
    fn qp_on(&self, cq: u32) -> Option<u32> {
        self.resources.iter().find_map(|(id, r)| match r {
            Resource::Qp { cq: on, .. } if *on == cq => Some(*id),
            _ => None,
        })
    }

    /// The connection manager id with handle `raw`.
    fn cm_id_of(&self, raw: usize) -> Option<u32> {
        self.resources.iter().find_map(|(id, r)| match r {
            Resource::CmId { raw: own, .. } if *own == raw => Some(*id),
            _ => None,
        })
    }

    /// Deliver a connection manager event about `id` on its channel.
    fn cm_event(&mut self, id: u32, kind: i32, listen: Option<u32>, data: &[u8]) {
        let listen_raw = listen.and_then(|l| match self.resources.get(&l) {
            Some(Resource::CmId { raw, .. }) => Some(*raw),
            _ => None,
        });
        let Some(Resource::CmId { channel, raw, .. }) = self.resources.get(&id) else {
            return;
        };
        let mut event = CmEvent {
            handle: *raw as *mut c_void,
            id: *raw as *mut c_void,
            listen_id: listen_raw.map_or(std::ptr::null_mut(), |r| r as *mut c_void),
            kind,
            private_len: data.len().min(56) as u32,
            ..CmEvent::default()
        };
        event.private[..data.len().min(56)].copy_from_slice(&data[..data.len().min(56)]);
        let channel = *channel;
        if let Some(Resource::CmChannel { fd, events, .. }) = self.resources.get_mut(&channel) {
            events.push_back(event);
            signal(fd);
        }
    }

    /// The endpoint a connection manager QP reports to the fabric.
    fn cm_endpoint(&self, qp: u32) -> Option<Endpoint> {
        let device = self.device(qp)?;
        let d = self.device_info(device)?;
        Some(Endpoint {
            gid: d.ipv4_gid(),
            qpn: qp,
            psn: 0,
            mtu: 3,
            lid: 0,
            port: d.port,
            link_layer: 2,
        })
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
            gid: device.gid,
            mtu: 3,
            port: device.port,
            link_layer: 2,
            ..Port::EMPTY
        };
        match device.profile {
            Profile::Window => port.flags = PORT_MW | PORT_RC,
            Profile::Registration => {
                port.flags = PORT_RC;
                port.ipv4_gid = device.ipv4_gid();
                port.ipv4_index = 1;
            }
        }
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

/// Open a device by name from the current view. `cm` must match its profile.
unsafe extern "C" fn open(name: *const c_char, cm: c_int) -> *mut c_void {
    let Some(sim) = current() else {
        return std::ptr::null_mut();
    };
    if sim.world.borrow_mut().rejected(Operation::Open, 0) {
        return std::ptr::null_mut();
    }
    let name = unsafe { CStr::from_ptr(name) }.to_bytes();
    let Some(device) = sim
        .devices
        .iter()
        .find(|d| d.name.as_bytes() == name && (cm != 0) == (d.profile == Profile::Registration))
    else {
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

/// Report the device's capabilities by profile.
unsafe extern "C" fn query_caps(device: *mut c_void, _port: u8, out: *mut Caps) -> c_int {
    let Some(h) = (unsafe { handle(device) }) else {
        return -1;
    };
    let world = h.sim.world.borrow();
    let Some(d) = world.device_info(h.id) else {
        return -1;
    };
    let caps = match d.profile {
        Profile::Window => Caps {
            flags: CAP_MW | if world.no_implicit_odp { 0 } else { CAP_ODP },
            max_msg_sz: 1 << 30,
            ipv4_index: -1,
            ipv4: [0; 4],
            max_qp_wr: world.device_limits.0,
            max_cqe: world.device_limits.1,
            max_mw: world.device_limits.2,
        },
        Profile::Registration => Caps {
            flags: 0,
            max_msg_sz: world.max_message.unwrap_or(1 << 30),
            ipv4_index: 1,
            ipv4: d.ipv4(),
            max_qp_wr: world.device_limits.0,
            max_cqe: world.device_limits.1,
            max_mw: world.device_limits.2,
        },
    };
    unsafe { *out = caps };
    0
}

/// Create a completion channel signaled through an eventfd.
unsafe extern "C" fn comp_channel(device: *mut c_void) -> *mut c_void {
    let Some(h) = (unsafe { handle(device) }) else {
        return std::ptr::null_mut();
    };
    allocate(
        &h.sim,
        Resource::CompChannel {
            device: h.id,
            fd: eventfd(),
            events: VecDeque::new(),
        },
    )
}

/// The channel's eventfd.
unsafe extern "C" fn comp_channel_fd(channel: *mut c_void) -> c_int {
    let Some(h) = (unsafe { handle(channel) }) else {
        return -1;
    };
    match h.sim.world.borrow().resources.get(&h.id) {
        Some(Resource::CompChannel { fd, .. }) => fd.as_raw_fd(),
        _ => -1,
    }
}

/// Free a completion channel. Fails while a CQ uses it.
unsafe extern "C" fn comp_channel_free(channel: *mut c_void) -> c_int {
    let Some(h) = (unsafe { handle(channel) }) else {
        return -1;
    };
    let mut world = h.sim.world.borrow_mut();
    if world
        .resources
        .values()
        .any(|r| matches!(r, Resource::Cq { channel: Some(c), .. } if *c == h.id))
    {
        return -1;
    }
    world.resources.remove(&h.id);
    drop(world);
    unsafe { drop(Box::from_raw(channel.cast::<Handle>())) };
    0
}

/// Create a CQ on `channel` (may be null).
unsafe extern "C" fn cq_create(
    device: *mut c_void,
    depth: u32,
    channel: *mut c_void,
) -> *mut c_void {
    let Some(h) = (unsafe { handle(device) }) else {
        return std::ptr::null_mut();
    };
    let channel = unsafe { handle(channel) }.map(|c| c.id);
    let mut world = h.sim.world.borrow_mut();
    if world.rejected(Operation::Cq, h.id) {
        return std::ptr::null_mut();
    }
    if depth == 0
        || world.device_info(h.id).is_none()
        || channel.is_some_and(|c| world.device(c) != Some(h.id))
    {
        world.record(Operation::Cq, h.id, None, -1, false);
        return std::ptr::null_mut();
    }
    let id = world.insert(Resource::Cq {
        device: h.id,
        channel,
        armed: false,
        raw: 0,
    });
    let raw = boxed(&h.sim, id);
    if let Some(Resource::Cq { raw: own, .. }) = world.resources.get_mut(&id) {
        *own = raw as usize;
    }
    world.record(Operation::Cq, h.id, None, 0, false);
    raw
}

/// Free a CQ. Fails while a QP uses it.
unsafe extern "C" fn cq_free(raw: *mut c_void) -> c_int {
    unsafe { free(raw, Operation::QpFree) }
}

/// Arm a CQ.
unsafe extern "C" fn req_notify(cq: *mut c_void) -> c_int {
    let Some(h) = (unsafe { handle(cq) }) else {
        return -1;
    };
    match h.sim.world.borrow_mut().resources.get_mut(&h.id) {
        Some(Resource::Cq { armed, .. }) => {
            *armed = true;
            0
        }
        _ => -1,
    }
}

/// Take one channel event without blocking.
unsafe extern "C" fn get_cq_event(channel: *mut c_void, cq: *mut *mut c_void) -> c_int {
    let Some(h) = (unsafe { handle(channel) }) else {
        return -libc::EINVAL;
    };
    let mut world = h.sim.world.borrow_mut();
    let Some(Resource::CompChannel { fd, events, .. }) = world.resources.get_mut(&h.id) else {
        return -libc::EINVAL;
    };
    let Some(raw) = events.pop_front() else {
        return -libc::EAGAIN;
    };
    consume(fd);
    unsafe { *cq = raw as *mut c_void };
    0
}

/// Nothing to count.
unsafe extern "C" fn ack_cq_events(_: *mut c_void, _: u32) {}

/// Create a window-mode QP on the device's port and CQ. Writes its QPN.
unsafe extern "C" fn qp(
    device: *mut c_void,
    cq: *mut c_void,
    port: u8,
    _: u32,
    _: u32,
    qpn: *mut u32,
) -> *mut c_void {
    let Some(h) = (unsafe { handle(device) }) else {
        return std::ptr::null_mut();
    };
    let Some(c) = (unsafe { handle(cq) }) else {
        return std::ptr::null_mut();
    };
    let mut world = h.sim.world.borrow_mut();
    if world.rejected(Operation::Qp, h.id) {
        return std::ptr::null_mut();
    }
    if !matches!(world.resources.get(&h.id), Some(Resource::Device(d)) if d.port == port)
        || !matches!(world.resources.get(&c.id), Some(Resource::Cq { device, .. }) if *device == h.id)
        || world.qp_on(c.id).is_some()
    {
        world.record(Operation::Qp, h.id, None, -1, false);
        return std::ptr::null_mut();
    }
    let id = world.insert(Resource::Qp {
        device: h.id,
        cq: c.id,
        alias: 0,
        local: None,
        remote: None,
        error: false,
        work: VecDeque::new(),
        recvs: VecDeque::new(),
        cq_entries: VecDeque::new(),
    });
    if let Some(Resource::Qp { alias, .. }) = world.resources.get_mut(&id) {
        *alias = id;
    }
    unsafe {
        *qpn = id;
    }
    world.record(Operation::Qp, h.id, None, 0, false);
    boxed(&h.sim, id)
}

/// Connect a window-mode QP once. Both endpoints must match real fabric
/// resources.
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
        if !matches!(world.resources.get(&remote.qpn), Some(Resource::Qp { .. })) {
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

/// Move a QP to the error state. Not traced.
unsafe extern "C" fn qp_error(raw: *mut c_void) -> c_int {
    let Some(h) = (unsafe { handle(raw) }) else {
        return -1;
    };
    let mut world = h.sim.world.borrow_mut();
    if !matches!(world.resources.get(&h.id), Some(Resource::Qp { .. })) {
        return -1;
    }
    world.set_error(h.id);
    0
}

/// Destroy a QP: drop its queued work, receives, and completions, unbind
/// its windows, and free it. No CQEs are produced. Traced as `Stop`.
unsafe extern "C" fn qp_free(raw: *mut c_void) -> c_int {
    let Some(h) = (unsafe { handle(raw) }) else {
        return -1;
    };
    let mut world = h.sim.world.borrow_mut();
    let result = world.call(Operation::Stop, h.id, |world| {
        if !matches!(world.resources.get(&h.id), Some(Resource::Qp { .. })) {
            return -1;
        }
        for resource in world.resources.values_mut() {
            match resource {
                Resource::Window { grant, .. } if grant.is_some_and(|g| g.qp == h.id) => {
                    *grant = None;
                }
                Resource::CmId { qp, .. } if *qp == Some(h.id) => *qp = None,
                _ => {}
            }
        }
        0
    });
    if result != 0 {
        return result;
    }
    world.resources.remove(&h.id);
    drop(world);
    unsafe { drop(Box::from_raw(raw.cast::<Handle>())) };
    0
}

/// Free a resource. Fails if anything still uses it.
unsafe fn free(raw: *mut c_void, operation: Operation) -> c_int {
    let Some(h) = (unsafe { handle(raw) }) else {
        return -1;
    };
    let mut world = h.sim.world.borrow_mut();
    let result = world.call(operation, h.id, |world| {
        let target = h.id;
        let referenced = world.resources.iter().any(|(id, resource)| {
            if *id == target {
                return false;
            }
            match resource {
                Resource::Qp {
                    device,
                    cq,
                    work,
                    recvs,
                    ..
                } => {
                    *device == target
                        || *cq == target
                        || work.iter().any(|w| w.action.uses(target))
                        || recvs.iter().any(|r| resource_of(r.sge.lkey) == target)
                }
                Resource::Cq {
                    device, channel, ..
                } => *device == target || *channel == Some(target),
                Resource::CompChannel { device, .. }
                | Resource::CmChannel { device, .. }
                | Resource::Region { device, .. } => *device == target,
                Resource::Mr { device, region, .. } => {
                    *device == target || region.is_some_and(|(r, _)| r == target)
                }
                Resource::Window { device, grant, .. } => {
                    *device == target || grant.is_some_and(|g| g.region == target || g.qp == target)
                }
                Resource::CmId {
                    channel,
                    device,
                    qp,
                    ..
                } => *channel == target || *device == Some(target) || *qp == Some(target),
                Resource::Device(_) => false,
            }
        });
        if referenced
            || matches!(
                world.resources.get(&target),
                Some(Resource::Window { grant: Some(_), .. })
            )
        {
            return -1;
        }
        world.resources.remove(&target);
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

/// Free a region. Fails while work, a window, or a registration uses it.
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
    let window = world
        .device_info(h.id)
        .is_some_and(|d| d.profile == Profile::Window);
    if length == 0
        || usize::try_from(length).is_err()
        || !matches!(access & !MR_REMOTE_WRITE, MR_PAGES | MR_CONTROL)
        // No windows: nothing to bind with.
        || (!window && access & MR_PAGES != 0)
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
            bytes: Memory::Host(vec![0; length as usize].into_boxed_slice()),
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

/// Ranges passed to [`prefetch`], from any thread, as `(address, length)`.
static PREFETCHED: std::sync::Mutex<Vec<(u64, u32)>> = std::sync::Mutex::new(Vec::new());

/// Prefetch into an implicit region. Callable from any thread, so it never
/// touches the thread-local world: it only logs the range for
/// [`prefetched`]. The simulated NIC never faults.
unsafe extern "C" fn prefetch(_: *mut c_void, address: u64, length: u32) -> c_int {
    PREFETCHED
        .lock()
        .unwrap_or_else(std::sync::PoisonError::into_inner)
        .push((address, length));
    0
}

/// True if a prefetch of exactly `[address, address + length)` was logged.
pub fn prefetched(address: u64, length: u32) -> bool {
    PREFETCHED
        .lock()
        .unwrap_or_else(std::sync::PoisonError::into_inner)
        .contains(&(address, length))
}

/// Register an implicit on-demand paging region. Rust checks the device's
/// capabilities first. Traced and faulted as `Register`.
unsafe extern "C" fn register_implicit(device: *mut c_void) -> *mut c_void {
    let Some(h) = (unsafe { handle(device) }) else {
        return std::ptr::null_mut();
    };
    let mut world = h.sim.world.borrow_mut();
    if world.no_implicit_odp
        || !matches!(world.device_info(h.id), Some(d) if d.profile == Profile::Window)
    {
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
            bytes: Memory::Host(Box::default()),
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

/// Map a dma-buf as a page region whose wire address is `iova`. Like C,
/// `fd` is not kept: the mapping holds its own reference.
unsafe extern "C" fn register_dmabuf(
    device: *mut c_void,
    offset: u64,
    length: u64,
    iova: u64,
    fd: c_int,
) -> *mut c_void {
    let Some(h) = (unsafe { handle(device) }) else {
        return std::ptr::null_mut();
    };
    let mut world = h.sim.world.borrow_mut();
    if world.rejected(Operation::RegisterDmabuf, h.id) {
        return std::ptr::null_mut();
    }
    let mapped = (length != 0 && fd >= 0 && usize::try_from(length).is_ok())
        .then(|| map(fd, offset, length as usize))
        .flatten();
    let Some(pointer) = mapped else {
        world.record(Operation::RegisterDmabuf, h.id, None, -1, false);
        return std::ptr::null_mut();
    };
    drop(world);
    let raw = allocate(
        &h.sim,
        Resource::Region {
            device: h.id,
            bytes: Memory::Mapped {
                pointer,
                length: length as usize,
            },
            address: iova,
            access: MR_PAGES | MR_DEVICE,
        },
    );
    h.sim
        .world
        .borrow_mut()
        .record(Operation::RegisterDmabuf, h.id, None, 0, false);
    raw
}

/// A region's bytes: owned host memory, or a shared mapping of a dma-buf.
enum Memory {
    Host(Box<[u8]>),

    Mapped { pointer: NonNull<u8>, length: usize },
}

impl std::ops::Deref for Memory {
    type Target = [u8];

    /// The region's bytes.
    fn deref(&self) -> &[u8] {
        match self {
            Self::Host(bytes) => bytes,
            // SAFETY: the mapping lives until this value drops.
            Self::Mapped { pointer, length } => unsafe {
                std::slice::from_raw_parts(pointer.as_ptr(), *length)
            },
        }
    }
}

impl std::ops::DerefMut for Memory {
    /// The region's bytes, writable.
    fn deref_mut(&mut self) -> &mut [u8] {
        match self {
            Self::Host(bytes) => bytes,
            // SAFETY: the mapping lives until this value drops.
            Self::Mapped { pointer, length } => unsafe {
                std::slice::from_raw_parts_mut(pointer.as_ptr(), *length)
            },
        }
    }
}

impl Drop for Memory {
    /// Unmap a dma-buf mapping. The owner's memory stays.
    fn drop(&mut self) {
        if let Self::Mapped { pointer, length } = self {
            unsafe {
                libc::munmap(pointer.as_ptr().cast(), *length);
            }
        }
    }
}

/// Map `length` bytes of `fd` from `offset`, shared and writable.
fn map(fd: c_int, offset: u64, length: usize) -> Option<NonNull<u8>> {
    let offset = libc::off_t::try_from(offset).ok()?;
    let pointer = unsafe {
        libc::mmap(
            std::ptr::null_mut(),
            length,
            libc::PROT_READ | libc::PROT_WRITE,
            libc::MAP_SHARED,
            fd,
            offset,
        )
    };
    if pointer == libc::MAP_FAILED {
        return None;
    }
    NonNull::new(pointer.cast())
}

/// Host memory standing in for a GPU dma-buf: a memfd the simulation maps.
///
/// The simulated NIC writes through its own shared mapping, so
/// [`Self::read`] sees exactly what a peer wrote.
pub struct MemfdMemory {
    fd: std::os::fd::OwnedFd,

    view: NonNull<u8>,

    length: usize,

    iova: u64,
}

// SAFETY: the mapping is plain shared memory, valid for this value's lifetime.
unsafe impl Send for MemfdMemory {}

unsafe impl Sync for MemfdMemory {}

impl MemfdMemory {
    /// A zeroed memfd of `length` bytes that windows address from `iova`.
    pub fn new(length: usize, iova: u64) -> std::io::Result<Arc<Self>> {
        if length == 0 {
            return Err(std::io::ErrorKind::InvalidInput.into());
        }
        let raw = unsafe { libc::memfd_create(c"rdma-verbs-sim".as_ptr(), libc::MFD_CLOEXEC) };
        if raw < 0 {
            return Err(std::io::Error::last_os_error());
        }
        let fd = unsafe { std::os::fd::OwnedFd::from_raw_fd(raw) };
        if unsafe { libc::ftruncate(fd.as_raw_fd(), length as libc::off_t) } != 0 {
            return Err(std::io::Error::last_os_error());
        }
        let view = map(fd.as_raw_fd(), 0, length).ok_or_else(std::io::Error::last_os_error)?;
        Ok(Arc::new(Self {
            fd,
            view,
            length,
            iova,
        }))
    }

    /// Copy `length` bytes at `offset` out, as a GPU kernel would read them.
    pub fn read(&self, offset: usize, length: usize) -> Vec<u8> {
        assert!(
            offset
                .checked_add(length)
                .is_some_and(|end| end <= self.length)
        );
        unsafe { std::slice::from_raw_parts(self.view.as_ptr().add(offset), length) }.to_vec()
    }
}

impl Drop for MemfdMemory {
    /// Unmap this value's view. The memfd closes with `fd`.
    fn drop(&mut self) {
        unsafe {
            libc::munmap(self.view.as_ptr().cast(), self.length);
        }
    }
}

impl crate::DmabufMemory for MemfdMemory {
    /// The memfd.
    fn fd(&self) -> std::os::fd::BorrowedFd<'_> {
        use std::os::fd::AsFd;

        self.fd.as_fd()
    }

    /// Always 0.
    fn offset(&self) -> u64 {
        0
    }

    /// The memfd size.
    fn length(&self) -> usize {
        self.length
    }

    /// The iova given to [`Self::new`].
    fn iova(&self) -> u64 {
        self.iova
    }
}

/// Pointer to a host region's bytes. Valid until the region is freed. Null
/// for an implicit or dma-buf region, like C.
unsafe extern "C" fn bytes(raw: *mut c_void) -> *mut u8 {
    let Some(h) = (unsafe { handle(raw) }) else {
        return std::ptr::null_mut();
    };
    match h.sim.world.borrow_mut().resources.get_mut(&h.id) {
        Some(Resource::Region {
            bytes: Memory::Host(bytes),
            access,
            ..
        }) if *access & MR_IMPLICIT == 0 => bytes.as_mut_ptr(),
        _ => std::ptr::null_mut(),
    }
}

/// A region's local key: `id << 8`.
unsafe extern "C" fn lkey(raw: *mut c_void) -> u32 {
    unsafe { handle(raw) }.map_or(0, |h| h.id << 8)
}

/// Register caller memory. Inside a region, the registration's wire
/// address follows the region's. Keys are `id << 8`.
unsafe extern "C" fn reg_mr(
    device: *mut c_void,
    addr: u64,
    length: u64,
    access: u32,
    lkey: *mut u32,
    rkey: *mut u32,
) -> *mut c_void {
    let Some(h) = (unsafe { handle(device) }) else {
        return std::ptr::null_mut();
    };
    let mut world = h.sim.world.borrow_mut();
    if world.rejected(Operation::RegMr, h.id) {
        return std::ptr::null_mut();
    }
    let end = addr.checked_add(length);
    if length == 0
        || end.is_none()
        || access & !MR_REMOTE_WRITE != MR_CONTROL
        || world.device_info(h.id).is_none()
    {
        world.record(Operation::RegMr, h.id, None, -1, false);
        return std::ptr::null_mut();
    }
    let region = world.resources.iter().find_map(|(id, r)| match r {
        Resource::Region {
            device,
            bytes: Memory::Host(bytes),
            address,
            access,
        } if *device == h.id && access & MR_IMPLICIT == 0 => {
            let base = bytes.as_ptr() as u64;
            (addr >= base && end.unwrap() <= base + bytes.len() as u64)
                .then(|| (*id, addr - base, *address))
        }
        _ => None,
    });
    let iova = region.map_or(addr, |(_, offset, address)| address + offset);
    let id = world.insert(Resource::Mr {
        device: h.id,
        addr,
        length,
        iova,
        access,
        region: region.map(|(id, offset, _)| (id, offset)),
    });
    unsafe {
        *lkey = id << 8;
        *rkey = id << 8;
    }
    world.record(Operation::RegMr, h.id, None, 0, false);
    boxed(&h.sim, id)
}

/// Deregister caller memory. Fails while queued work uses it.
unsafe extern "C" fn dereg_mr(raw: *mut c_void) -> c_int {
    unsafe { free(raw, Operation::DeregMr) }
}

/// Create an unbound window. Its key is `id << 8 | 1`. Window profile only.
unsafe extern "C" fn window(device: *mut c_void, key: *mut u32) -> *mut c_void {
    let Some(h) = (unsafe { handle(device) }) else {
        return std::ptr::null_mut();
    };
    if h.sim.world.borrow_mut().rejected(Operation::Window, h.id) {
        return std::ptr::null_mut();
    }
    if !matches!(h.sim.world.borrow().device_info(h.id), Some(d) if d.profile == Profile::Window) {
        h.sim
            .world
            .borrow_mut()
            .record(Operation::Window, h.id, None, -1, false);
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

/// Copy `n` entries, or `None` for a null list.
unsafe fn sges(list: *const Sge, n: u32) -> Option<Vec<Sge>> {
    if list.is_null() {
        return None;
    }
    Some(unsafe { std::slice::from_raw_parts(list, n as usize) }.to_vec())
}

/// Queue an RDMA write. Checked when it completes.
#[allow(clippy::too_many_arguments)]
unsafe extern "C" fn post_write(
    qp: *mut c_void,
    list: *const Sge,
    n: u32,
    address: u64,
    key: u32,
    imm: u32,
    flags: u32,
    id: u64,
) -> c_int {
    let (Some(q), Some(sges)) = (unsafe { (handle(qp), sges(list, n)) }) else {
        return -1;
    };
    q.sim.world.borrow_mut().post(
        q.id,
        id,
        Action::Write {
            sges,
            address,
            key,
            imm: (flags & POST_IMM != 0).then_some(imm),
            signaled: flags & POST_SIGNALED != 0,
        },
    )
}

/// Queue a send. Delivered when it completes.
unsafe extern "C" fn post_send(qp: *mut c_void, list: *const Sge, n: u32, id: u64) -> c_int {
    let (Some(q), Some(sges)) = (unsafe { (handle(qp), sges(list, n)) }) else {
        return -1;
    };
    q.sim
        .world
        .borrow_mut()
        .post(q.id, id, Action::Send { sges })
}

/// Post a receive into one entry.
unsafe extern "C" fn post_recv(qp: *mut c_void, list: *const Sge, n: u32, id: u64) -> c_int {
    let (Some(q), Some(sges)) = (unsafe { (handle(qp), sges(list, n)) }) else {
        return -1;
    };
    let mut world = q.sim.world.borrow_mut();
    if sges.len() != 1 {
        world.record(Operation::PostRecv, q.id, Some(id), -1, false);
        return -1;
    }
    world.post_recv(q.id, Recv { id, sge: sges[0] })
}

/// Report delivered receive completions, then run up to 32 queued requests
/// in order, for the QP on `cq`. Stops at a delay or a failure. In the error
/// state, flushes. Traced under the QP's name.
unsafe extern "C" fn poll(cq: *mut c_void, out: *mut Completion, capacity: u32) -> c_int {
    let Some(c) = (unsafe { handle(cq) }) else {
        return -1;
    };
    let mut world = c.sim.world.borrow_mut();
    if !matches!(world.resources.get(&c.id), Some(Resource::Cq { .. })) {
        return -1;
    }
    let Some(qp) = world.qp_on(c.id) else {
        // No QP yet, or it was destroyed: nothing can complete.
        return 0;
    };
    if world.rejected(Operation::Poll, qp) {
        return -1;
    }
    let mut count = 0;
    while count < capacity.min(32) {
        let Some(Resource::Qp {
            work,
            recvs,
            cq_entries,
            error,
            ..
        }) = world.resources.get_mut(&qp)
        else {
            break;
        };
        let (operation, completion, executed, report) = if let Some(c) = cq_entries.pop_front() {
            (Operation::PostRecv, c, false, true)
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
            (operation, flushed, false, true)
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
                world.execute(qp, &pending)
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
            (
                pending.action.operation(),
                completion,
                true,
                status != 0 || pending.action.signaled(),
            )
        };
        if completion.status != 0 {
            world.set_error(qp);
        }
        if !report {
            continue;
        }
        world.record(
            operation,
            qp,
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
    // Work still queued (delayed, reordered, or past this batch) completes
    // later, as real hardware would raise an event then: re-raise one now
    // so an armed CQ is not left waiting for a completion it never hears of.
    let unfinished = matches!(
        world.resources.get(&qp),
        Some(Resource::Qp { work, recvs, cq_entries, error, .. })
            if !work.is_empty() || !cq_entries.is_empty() || (*error && !recvs.is_empty())
    );
    if unfinished {
        world.notify(qp);
    }
    world.record(Operation::Poll, qp, None, 0, false);
    count as c_int
}

/// Create a connection manager event channel signaled through an eventfd.
unsafe extern "C" fn cm_channel(device: *mut c_void) -> *mut c_void {
    let Some(h) = (unsafe { handle(device) }) else {
        return std::ptr::null_mut();
    };
    allocate(
        &h.sim,
        Resource::CmChannel {
            device: h.id,
            fd: eventfd(),
            events: VecDeque::new(),
        },
    )
}

/// The channel's eventfd.
unsafe extern "C" fn cm_channel_fd(channel: *mut c_void) -> c_int {
    let Some(h) = (unsafe { handle(channel) }) else {
        return -1;
    };
    match h.sim.world.borrow().resources.get(&h.id) {
        Some(Resource::CmChannel { fd, .. }) => fd.as_raw_fd(),
        _ => -1,
    }
}

/// Free a channel. Fails while an id uses it.
unsafe extern "C" fn cm_channel_free(channel: *mut c_void) -> c_int {
    let Some(h) = (unsafe { handle(channel) }) else {
        return -1;
    };
    let mut world = h.sim.world.borrow_mut();
    if world
        .resources
        .values()
        .any(|r| matches!(r, Resource::CmId { channel, .. } if *channel == h.id))
    {
        return -1;
    }
    world.resources.remove(&h.id);
    drop(world);
    unsafe { drop(Box::from_raw(channel.cast::<Handle>())) };
    0
}

/// A new id on `channel`, with its handle recorded.
fn cm_new_id(
    sim: &Simulation,
    world: &mut World,
    channel: u32,
    device: Option<u32>,
) -> (u32, usize) {
    let id = world.insert(Resource::CmId {
        channel,
        raw: 0,
        device,
        listen: None,
        destination: None,
        qp: None,
        peer: None,
        unacked: 0,
        established: false,
    });
    let raw = boxed(sim, id) as usize;
    if let Some(Resource::CmId { raw: own, .. }) = world.resources.get_mut(&id) {
        *own = raw;
    }
    (id, raw)
}

/// Listen on `addr`:`port`, which must be `device`'s IPv4 address and free.
unsafe extern "C" fn cm_listen(
    channel: *mut c_void,
    device: *mut c_void,
    addr: *const u8,
    port: u16,
    out: *mut *mut c_void,
) -> c_int {
    let (Some(c), Some(d)) = (unsafe { (handle(channel), handle(device)) }) else {
        return -1;
    };
    let addr: [u8; 4] = unsafe { std::slice::from_raw_parts(addr, 4) }
        .try_into()
        .unwrap();
    let mut world = c.sim.world.borrow_mut();
    if world.rejected(Operation::CmListen, d.id) {
        return -1;
    }
    let taken = world
        .resources
        .values()
        .any(|r| matches!(r, Resource::CmId { listen: Some(l), .. } if *l == (addr, port)));
    if taken || world.device_info(d.id).is_none_or(|i| i.ipv4() != addr) {
        world.record(Operation::CmListen, d.id, None, -1, false);
        return -1;
    }
    let (id, raw) = cm_new_id(&c.sim, &mut world, c.id, Some(d.id));
    if let Some(Resource::CmId { listen, .. }) = world.resources.get_mut(&id) {
        *listen = Some((addr, port));
    }
    unsafe { *out = raw as *mut c_void };
    world.record(Operation::CmListen, d.id, None, 0, false);
    0
}

/// Create an id on `channel`.
unsafe extern "C" fn cm_id(channel: *mut c_void, out: *mut *mut c_void) -> c_int {
    let Some(c) = (unsafe { handle(channel) }) else {
        return -1;
    };
    let mut world = c.sim.world.borrow_mut();
    if !matches!(world.resources.get(&c.id), Some(Resource::CmChannel { .. })) {
        return -1;
    }
    let (_, raw) = cm_new_id(&c.sim, &mut world, c.id, None);
    unsafe { *out = raw as *mut c_void };
    0
}

/// Resolve the address: bind the id to the channel's device, whose IPv4
/// address must be `src`.
unsafe extern "C" fn cm_resolve_addr(
    raw: *mut c_void,
    src: *const u8,
    dst: *const u8,
    port: u16,
    _: c_int,
) -> c_int {
    let Some(h) = (unsafe { handle(raw) }) else {
        return -1;
    };
    let src: [u8; 4] = unsafe { std::slice::from_raw_parts(src, 4) }
        .try_into()
        .unwrap();
    let dst: [u8; 4] = unsafe { std::slice::from_raw_parts(dst, 4) }
        .try_into()
        .unwrap();
    let mut world = h.sim.world.borrow_mut();
    let fault = world.fault(Operation::CmResolveAddr, h.id);
    let device = match world.resources.get(&h.id) {
        Some(Resource::CmId {
            channel,
            device: None,
            listen: None,
            ..
        }) => world.device(*channel),
        _ => None,
    };
    let valid = device.is_some_and(|d| world.device_info(d).is_some_and(|i| i.ipv4() == src));
    if !valid || fault == Some(Fault::Reject) {
        world.record(Operation::CmResolveAddr, h.id, None, -1, false);
        return -1;
    }
    if fault == Some(Fault::AddressError) {
        world.cm_event(h.id, registration::event::ADDR_ERROR, None, &[]);
    } else {
        if let Some(Resource::CmId {
            device: bound,
            destination,
            ..
        }) = world.resources.get_mut(&h.id)
        {
            *bound = device;
            *destination = Some((dst, port));
        }
        world.cm_event(h.id, registration::event::ADDR_RESOLVED, None, &[]);
    }
    world.record(Operation::CmResolveAddr, h.id, None, 0, false);
    0
}

/// Resolve the route of an id with a resolved address.
unsafe extern "C" fn cm_resolve_route(raw: *mut c_void, _: c_int) -> c_int {
    let Some(h) = (unsafe { handle(raw) }) else {
        return -1;
    };
    let mut world = h.sim.world.borrow_mut();
    let fault = world.fault(Operation::CmResolveRoute, h.id);
    let valid = matches!(
        world.resources.get(&h.id),
        Some(Resource::CmId {
            destination: Some(_),
            ..
        })
    );
    if !valid || fault == Some(Fault::Reject) {
        world.record(Operation::CmResolveRoute, h.id, None, -1, false);
        return -1;
    }
    let kind = if fault == Some(Fault::RouteError) {
        registration::event::ROUTE_ERROR
    } else {
        registration::event::ROUTE_RESOLVED
    };
    world.cm_event(h.id, kind, None, &[]);
    world.record(Operation::CmResolveRoute, h.id, None, 0, false);
    0
}

/// Create an id's QP on `cq`. The id must be bound to `device`. The QP is
/// named by the caller's `*qpn`, if nonzero.
unsafe extern "C" fn cm_create_qp(
    raw: *mut c_void,
    device: *mut c_void,
    cq: *mut c_void,
    _: u32,
    _: u32,
    qpn: *mut u32,
) -> *mut c_void {
    let (Some(h), Some(d), Some(c)) = (unsafe { (handle(raw), handle(device), handle(cq)) }) else {
        return std::ptr::null_mut();
    };
    let mut world = h.sim.world.borrow_mut();
    if world.rejected(Operation::CmCreateQp, h.id) {
        return std::ptr::null_mut();
    }
    let bound = matches!(
        world.resources.get(&h.id),
        Some(Resource::CmId { device: Some(b), qp: None, .. }) if *b == d.id
    );
    if !bound
        || world.device(c.id) != Some(d.id)
        || !matches!(world.resources.get(&c.id), Some(Resource::Cq { .. }))
        || world.qp_on(c.id).is_some()
    {
        world.record(Operation::CmCreateQp, h.id, None, -1, false);
        return std::ptr::null_mut();
    }
    let name = unsafe { *qpn };
    let id = world.insert(Resource::Qp {
        device: d.id,
        cq: c.id,
        alias: 0,
        local: None,
        remote: None,
        error: false,
        work: VecDeque::new(),
        recvs: VecDeque::new(),
        cq_entries: VecDeque::new(),
    });
    if let Some(Resource::Qp { alias, .. }) = world.resources.get_mut(&id) {
        *alias = if name != 0 { name } else { id };
    }
    if let Some(Resource::CmId { qp, .. }) = world.resources.get_mut(&h.id) {
        *qp = Some(id);
    }
    unsafe { *qpn = world.name(id) };
    world.record(Operation::CmCreateQp, h.id, None, 0, false);
    boxed(&h.sim, id)
}

/// Send a connection request to the listener on the resolved destination.
/// Without one the request is rejected.
unsafe extern "C" fn cm_connect(raw: *mut c_void, data: *const u8, len: u32) -> c_int {
    let Some(h) = (unsafe { handle(raw) }) else {
        return -1;
    };
    let data = unsafe { std::slice::from_raw_parts(data, len as usize) }.to_vec();
    let mut world = h.sim.world.borrow_mut();
    let fault = world.fault(Operation::CmConnect, h.id);
    let Some(Resource::CmId {
        destination: Some(destination),
        qp: Some(_),
        peer: None,
        ..
    }) = world.resources.get(&h.id)
    else {
        world.record(Operation::CmConnect, h.id, None, -1, false);
        return -1;
    };
    let destination = *destination;
    if len > 56 || fault == Some(Fault::Reject) {
        world.record(Operation::CmConnect, h.id, None, -1, false);
        return -1;
    }
    world.record(Operation::CmConnect, h.id, None, 0, false);
    if fault == Some(Fault::Timeout) {
        return 0;
    }
    let listener = world.resources.iter().find_map(|(id, r)| match r {
        Resource::CmId {
            listen: Some(l),
            channel,
            device,
            ..
        } if *l == destination => Some((*id, *channel, *device)),
        _ => None,
    });
    let Some((listener, channel, device)) = listener else {
        world.cm_event(h.id, registration::event::REJECTED, None, &[]);
        return 0;
    };
    let (request, _) = cm_new_id(&h.sim, &mut world, channel, device);
    if let Some(Resource::CmId { peer, .. }) = world.resources.get_mut(&request) {
        *peer = Some(h.id);
    }
    if let Some(Resource::CmId { peer, .. }) = world.resources.get_mut(&h.id) {
        *peer = Some(request);
    }
    world.cm_event(
        request,
        registration::event::CONNECT_REQUEST,
        Some(listener),
        &data,
    );
    0
}

/// Accept a request: connect both QPs and report `ESTABLISHED` to both sides.
unsafe extern "C" fn cm_accept(raw: *mut c_void, data: *const u8, len: u32) -> c_int {
    let Some(h) = (unsafe { handle(raw) }) else {
        return -1;
    };
    let data = unsafe { std::slice::from_raw_parts(data, len as usize) }.to_vec();
    let mut world = h.sim.world.borrow_mut();
    world.call(Operation::CmAccept, h.id, |world| {
        let Some(Resource::CmId {
            qp: Some(ours),
            peer: Some(peer),
            established: false,
            ..
        }) = world.resources.get(&h.id)
        else {
            return -1;
        };
        let (ours, peer) = (*ours, *peer);
        let Some(Resource::CmId {
            qp: Some(theirs), ..
        }) = world.resources.get(&peer)
        else {
            return -1;
        };
        let theirs = *theirs;
        let (Some(a), Some(b)) = (world.cm_endpoint(ours), world.cm_endpoint(theirs)) else {
            return -1;
        };
        for (qp, local, remote) in [(ours, a, b), (theirs, b, a)] {
            if let Some(Resource::Qp {
                local: l,
                remote: r,
                ..
            }) = world.resources.get_mut(&qp)
            {
                *l = Some(local);
                *r = Some(remote);
            }
        }
        for id in [h.id, peer] {
            if let Some(Resource::CmId { established, .. }) = world.resources.get_mut(&id) {
                *established = true;
            }
        }
        world.cm_event(peer, registration::event::ESTABLISHED, None, &data);
        world.cm_event(h.id, registration::event::ESTABLISHED, None, &[]);
        0
    })
}

/// Reject a request: the initiator sees `REJECTED`.
unsafe extern "C" fn cm_reject(raw: *mut c_void) -> c_int {
    let Some(h) = (unsafe { handle(raw) }) else {
        return -1;
    };
    let mut world = h.sim.world.borrow_mut();
    world.call(Operation::CmReject, h.id, |world| {
        let Some(Resource::CmId { peer, .. }) = world.resources.get_mut(&h.id) else {
            return -1;
        };
        let Some(initiator) = peer.take() else {
            return -1;
        };
        if let Some(Resource::CmId { peer, .. }) = world.resources.get_mut(&initiator) {
            *peer = None;
        }
        world.cm_event(initiator, registration::event::REJECTED, None, &[]);
        0
    })
}

/// Take one event without blocking.
unsafe extern "C" fn cm_get_event(channel: *mut c_void, out: *mut CmEvent) -> c_int {
    let Some(h) = (unsafe { handle(channel) }) else {
        return -libc::EINVAL;
    };
    let mut world = h.sim.world.borrow_mut();
    let Some(Resource::CmChannel { fd, events, .. }) = world.resources.get_mut(&h.id) else {
        return -libc::EINVAL;
    };
    let Some(event) = events.pop_front() else {
        return -libc::EAGAIN;
    };
    consume(fd);
    if let Some(id) = world.cm_id_of(event.id as usize)
        && let Some(Resource::CmId { unacked, .. }) = world.resources.get_mut(&id)
    {
        *unacked += 1;
    }
    unsafe { *out = event };
    0
}

/// Acknowledge an event.
unsafe extern "C" fn cm_ack(handle: *mut c_void) {
    let Some(h) = (unsafe { self::handle(handle) }) else {
        return;
    };
    let mut world = h.sim.world.borrow_mut();
    if let Some(id) = world.cm_id_of(handle as usize)
        && let Some(Resource::CmId { unacked, .. }) = world.resources.get_mut(&id)
    {
        *unacked = unacked.saturating_sub(1);
    }
}

/// Destroy an id. Fails (the real call would block) with unacknowledged
/// events, and fails while its QP exists. A connected peer sees
/// `DISCONNECTED`.
unsafe extern "C" fn cm_destroy_id(raw: *mut c_void) -> c_int {
    let Some(h) = (unsafe { handle(raw) }) else {
        return -1;
    };
    let mut world = h.sim.world.borrow_mut();
    let result = world.call(Operation::CmDestroyId, h.id, |world| {
        let Some(Resource::CmId {
            unacked: 0,
            qp: None,
            peer,
            established,
            ..
        }) = world.resources.get(&h.id)
        else {
            return -1;
        };
        let (peer, established) = (*peer, *established);
        if let Some(peer) = peer {
            if let Some(Resource::CmId { peer: back, .. }) = world.resources.get_mut(&peer) {
                *back = None;
            }
            let kind = if established {
                registration::event::DISCONNECTED
            } else {
                registration::event::REJECTED
            };
            world.cm_event(peer, kind, None, &[]);
        }
        // Events still queued for this id are discarded, like the kernel's.
        let raw = raw as usize;
        for resource in world.resources.values_mut() {
            if let Resource::CmChannel { fd, events, .. } = resource {
                let before = events.len();
                events.retain(|e| e.id as usize != raw);
                for _ in events.len()..before {
                    consume(fd);
                }
            }
        }
        world.resources.remove(&h.id);
        0
    });
    if result != 0 {
        return result;
    }
    drop(world);
    unsafe { drop(Box::from_raw(raw.cast::<Handle>())) };
    0
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
        let mut pointer = std::ptr::null_mut();
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
        let sge = Sge {
            addr: 0,
            length: 1,
            lkey: 0,
        };
        let mut caps = Caps::default();
        let mut event = CmEvent::default();
        let address = [1u8; 4];
        unsafe {
            assert!(handle(raw).is_none());
            assert!(qp(raw, raw, 1, 1, 1, &mut output).is_null());
            assert!(cq_create(raw, 1, raw).is_null());
            assert!(register(raw, 32, 5).is_null());
            assert!(reg_mr(raw, 1, 1, 2, &mut output, &mut output).is_null());
            assert!(window(raw, &mut output).is_null());
            assert!(bytes(raw).is_null());
            assert_eq!(lkey(raw), 0);
            assert_eq!(query_caps(raw, 1, &mut caps), -1);
            assert!(comp_channel(raw).is_null());
            assert_eq!(comp_channel_fd(raw), -1);
            assert_eq!(req_notify(raw), -1);
            assert_eq!(get_cq_event(raw, &mut pointer), -libc::EINVAL);
            assert_eq!(connect(raw, &endpoint, &endpoint), -1);
            assert_eq!(qp_error(raw), -1);
            assert_eq!(qp_free(raw), -1);
            assert_eq!(bind(raw, raw, raw, 1, 2, 0, 3), -1);
            assert_eq!(invalidate(raw, 1, 2), -1);
            assert_eq!(post_write(raw, &sge, 1, 2, 3, 4, 3, 5), -1);
            assert_eq!(post_send(raw, &sge, 1, 2), -1);
            assert_eq!(post_recv(raw, &sge, 1, 2), -1);
            assert_eq!(poll(raw, &mut completion, 1), -1);
            assert_eq!(close(raw), -1);
            assert_eq!(cq_free(raw), -1);
            assert_eq!(deregister(raw), -1);
            assert_eq!(dereg_mr(raw), -1);
            assert_eq!(window_free(raw), -1);
            assert_eq!(comp_channel_free(raw), -1);
            assert!(cm_channel(raw).is_null());
            assert_eq!(cm_channel_fd(raw), -1);
            assert_eq!(cm_channel_free(raw), -1);
            assert_eq!(cm_listen(raw, raw, address.as_ptr(), 1, &mut pointer), -1);
            assert_eq!(cm_id(raw, &mut pointer), -1);
            assert_eq!(
                cm_resolve_addr(raw, address.as_ptr(), address.as_ptr(), 1, 1),
                -1
            );
            assert_eq!(cm_resolve_route(raw, 1), -1);
            assert!(cm_create_qp(raw, raw, raw, 1, 1, &mut output).is_null());
            assert_eq!(cm_connect(raw, address.as_ptr(), 4), -1);
            assert_eq!(cm_accept(raw, address.as_ptr(), 4), -1);
            assert_eq!(cm_reject(raw), -1);
            assert_eq!(cm_get_event(raw, &mut event), -libc::EINVAL);
            cm_ack(raw);
            assert_eq!(cm_destroy_id(raw), -1);
        }
        assert_eq!(output, 123);
        assert!(pointer.is_null());
        assert_eq!(
            (completion.id, completion.status, completion.opcode),
            (42, 43, 44)
        );
    }

    #[test]
    /// Every allocation failure path can be passed back without a null access.
    fn allocation_failures_reject_null_handles_without_side_effects() {
        assert!(current().is_none());
        null_handle_calls_fail(unsafe { open(c"sim0".as_ptr(), 0) });
        let sim = Simulation::new()
            .with_devices(vec![Device::new("sim0", [1; 16])])
            .unwrap();
        let _scope = sim.enter();
        unsafe {
            let device = open(c"sim0".as_ptr(), 0);
            assert!(handle(device).is_some());
            let cq = cq_create(device, 4, std::ptr::null_mut());
            let mut output = 123;
            let mut failures = vec![
                open(c"missing".as_ptr(), 0),
                // The window profile does not open through the connection manager.
                open(c"sim0".as_ptr(), 1),
                qp(device, cq, 2, 1, 1, &mut output),
                register(device, 0, 5),
                cq_create(device, 0, std::ptr::null_mut()),
            ];
            sim.fault(Operation::Open, Fault::Reject);
            failures.push(open(c"sim0".as_ptr(), 0));
            sim.fault(Operation::Qp, Fault::Reject);
            failures.push(qp(device, cq, 1, 1, 1, &mut output));
            sim.fault(Operation::Cq, Fault::Reject);
            failures.push(cq_create(device, 4, std::ptr::null_mut()));
            sim.fault(Operation::Register, Fault::Reject);
            failures.push(register(device, 32, 5));
            sim.fault(Operation::RegMr, Fault::Reject);
            failures.push(reg_mr(device, 4096, 64, 2, &mut output, &mut output));
            sim.fault(Operation::Window, Fault::Reject);
            failures.push(window(device, &mut output));
            assert_eq!(output, 123);
            assert_eq!(sim.pending_faults(), 0);
            let trace = sim.trace();
            sim.fault(Operation::Stop, Fault::Reject);
            for raw in failures {
                null_handle_calls_fail(raw);
                assert_eq!(sim.live_resources(), 2);
                assert_eq!(sim.trace(), trace);
                assert_eq!(sim.pending_faults(), 1);
            }
            assert_eq!(cq_free(cq), 0);
            assert_eq!(close(device), 0);
        }
        assert_eq!(sim.live_resources(), 0);
    }

    #[test]
    /// A null secondary handle cannot post work or consume a scripted fault.
    fn posts_reject_each_null_handle_without_side_effects() {
        let sim = Simulation::new();
        {
            let link = link(&sim, |_| {});
            let (window, key) = grant(&link, wr(1), 4096, 17);
            poke(&link.pages, 0, &[0xa5; 17]);
            let q = link.a.conn.qp.get().unwrap().as_ptr();
            let w = window.raw.unwrap().as_ptr();
            let r = link.pages.raw.unwrap().as_ptr();
            let address = link.pages.address() + 4096;
            let sge = link.pages.sge(0, 17);
            let null = std::ptr::null_mut();
            let trace = sim.trace();
            let live = sim.live_resources();
            sim.fault(Operation::Bind, Fault::Reject);
            sim.fault(Operation::WriteImm, Fault::Reject);
            unsafe {
                for (q, w, r) in [(null, w, r), (q, null, r), (q, w, null)] {
                    assert_eq!(bind(q, w, r, inc_rkey(key), 100, 4096, 17), -1);
                }
                assert_eq!(
                    post_write(null, &sge, 1, address, key, 0, POST_IMM, 101),
                    -1
                );
                assert_eq!(
                    post_write(q, std::ptr::null(), 1, address, key, 0, POST_IMM, 101),
                    -1
                );
            }
            assert_eq!(sim.trace(), trace);
            assert_eq!(sim.live_resources(), live);
            assert_eq!(sim.pending_faults(), 2);
            assert_eq!(drain(&link.a).unwrap(), []);
            stop_both(&link);
            assert_eq!(peek(&link.pages, 0, 17), [0xa5; 17]);
            assert_eq!(peek(&link.pages, 4096, 17), [0; 17]);
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
            assert!(open(c"missing".as_ptr(), 0).is_null());
            events(&sim, &[(Operation::Open, 0, None, -1, false)]);
            assert_eq!(sim.live_resources(), 0);
            let device = open(c"sim0".as_ptr(), 0);
            assert!(!device.is_null());
            events(&sim, &[(Operation::Open, 0, None, 0, false)]);
            let d = handle(device).unwrap().id;
            let cq = cq_create(device, 4, std::ptr::null_mut());
            events(&sim, &[(Operation::Cq, d, None, 0, false)]);
            let mut id = 0;
            assert!(qp(device, cq, 2, 1, 1, &mut id).is_null());
            assert_eq!(id, 0);
            events(&sim, &[(Operation::Qp, d, None, -1, false)]);
            assert!(register(device, 0, 5).is_null());
            events(&sim, &[(Operation::Register, d, None, -1, false)]);
            assert_eq!(sim.live_resources(), 2);
            for operation in [
                Operation::Discover,
                Operation::Open,
                Operation::Cq,
                Operation::Qp,
                Operation::Register,
                Operation::RegMr,
                Operation::Window,
            ] {
                sim.fault(operation, Fault::Reject);
                let resource = match operation {
                    Operation::Discover => {
                        assert_eq!(discover(ports.as_mut_ptr(), 1), -1);
                        0
                    }
                    Operation::Open => {
                        assert!(open(c"sim0".as_ptr(), 0).is_null());
                        0
                    }
                    Operation::Cq => {
                        assert!(cq_create(device, 4, std::ptr::null_mut()).is_null());
                        d
                    }
                    Operation::Qp => {
                        assert!(qp(device, cq, 1, 1, 1, &mut id).is_null());
                        d
                    }
                    Operation::Register => {
                        assert!(register(device, 32, 5).is_null());
                        d
                    }
                    Operation::RegMr => {
                        assert!(reg_mr(device, 4096, 64, 2, &mut id, &mut id).is_null());
                        d
                    }
                    Operation::Window => {
                        assert!(window(device, &mut id).is_null());
                        d
                    }
                    _ => unreachable!(),
                };
                events(&sim, &[(operation, resource, None, -1, false)]);
                assert_eq!(sim.live_resources(), 2);
                assert_eq!(sim.pending_faults(), 0);
            }
            assert_eq!(qp_free(device), -1);
            events(&sim, &[(Operation::Stop, d, None, -1, false)]);
            assert_eq!(close(device), -1);
            events(&sim, &[(Operation::Close, d, None, -1, false)]);
            assert_eq!(cq_free(cq), 0);
            assert_eq!(close(device), 0);
            sim.take_trace();
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
            let device = open(c"sim0".as_ptr(), 0);
            let d = handle(device).unwrap().id;
            let (cq_a, cq_b) = (
                cq_create(device, 4, std::ptr::null_mut()),
                cq_create(device, 4, std::ptr::null_mut()),
            );
            let mut qpn = 0;
            let q = qp(device, cq_a, 1, 1, 1, &mut qpn);
            let local = Endpoint {
                gid: [1; 16],
                qpn,
                psn: 0,
                mtu: 3,
                lid: 0,
                port: 1,
                link_layer: 2,
            };
            let peer = qp(device, cq_b, 1, 1, 1, &mut qpn);
            let remote = Endpoint { qpn, ..local };
            // One CQ per QP.
            assert!(qp(device, cq_a, 1, 1, 1, &mut qpn).is_null());
            let region = register(device, 32, 5);
            let r = handle(region).unwrap().id;
            let mut key = 0;
            let w = window(device, &mut key);
            events(
                &sim,
                &[
                    (Operation::Open, 0, None, 0, false),
                    (Operation::Cq, d, None, 0, false),
                    (Operation::Cq, d, None, 0, false),
                    (Operation::Qp, d, None, 0, false),
                    (Operation::Qp, d, None, 0, false),
                    (Operation::Qp, d, None, -1, false),
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
                assert_eq!(sim.live_resources(), 7);
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
                    assert_eq!(poll(cq_a, &mut completion, 1), 1);
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
                    (cq_a, Operation::QpFree),
                    (region, Operation::Deregister),
                    (w, Operation::WindowFree),
                ] {
                    let id = handle(raw).unwrap().id;
                    assert_eq!(free(raw, operation), -1);
                    events(&sim, &[(operation, id, None, -1, false)]);
                    assert_eq!(handle(raw).unwrap().id, id);
                    assert!(sim.world.borrow().resources.contains_key(&id));
                    assert_eq!(sim.live_resources(), 7);
                }
            }
            sim.fault(Operation::Stop, Fault::Reject);
            assert_eq!(qp_free(q), -1);
            events(&sim, &[(Operation::Stop, local.qpn, None, -1, false)]);
            assert!(sim.world.borrow().ready(local.qpn));
            assert_eq!(qp_free(q), 0);
            events(&sim, &[(Operation::Stop, local.qpn, None, 0, false)]);
            assert!(!sim.world.borrow().ready(local.qpn));
            assert_eq!(qp_free(peer), 0);
            events(&sim, &[(Operation::Stop, remote.qpn, None, 0, false)]);
            for (raw, operation) in [
                (w, Operation::WindowFree),
                (region, Operation::Deregister),
                (cq_a, Operation::QpFree),
                (cq_b, Operation::QpFree),
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
            let link = link(&sim, |_| {});
            let other = self::link(&foreign, |_| {});
            let (w, key) = grant(&link, wr(1), 4096, 17);
            poke(&link.pages, 0, &[0xa5; 17]);
            sim.take_trace();
            foreign.take_trace();
            let q = link.a.conn.qp.get().unwrap().as_ptr();
            let cq = link.a.cq.as_ptr();
            let qpn = link.a.endpoint.qpn;
            let target = link.pages.address() + 4096;
            let w = w.raw.unwrap().as_ptr();
            unsafe {
                for (region, key) in [
                    (other.pages.raw.unwrap().as_ptr(), inc_rkey(key)),
                    (link.pages.raw.unwrap().as_ptr(), key + 0x100),
                ] {
                    assert_eq!(bind(q, w, region, key, 41, 0, 17), -1);
                    events(&sim, &[(Operation::Bind, qpn, Some(41), -1, false)]);
                }
                // A source from another fabric, and one past the region's end.
                let foreign_sge = other.pages.sge(0, 17);
                assert_eq!(
                    post_write(
                        q,
                        &foreign_sge,
                        1,
                        target,
                        key,
                        0,
                        POST_IMM | POST_SIGNALED,
                        42
                    ),
                    -1
                );
                events(&sim, &[(Operation::WriteImm, qpn, Some(42), -1, false)]);
                let past = link.pages.sge(8190, 17);
                assert_eq!(
                    post_write(q, &past, 1, target, key, 0, POST_IMM | POST_SIGNALED, 43),
                    -1
                );
                events(&sim, &[(Operation::WriteImm, qpn, Some(43), -1, false)]);
                assert_eq!(poll(cq, std::ptr::null_mut(), 0), 0);
                events(&sim, &[(Operation::Poll, qpn, None, 0, false)]);
                // The failed write moves the QP to the error state, so the
                // invalidate behind it is flushed rather than executed.
                let source = link.pages.sge(0, 17);
                for (operation, id, status) in [
                    (Operation::WriteImm, 44, wc::REM_ACCESS_ERR),
                    (Operation::Invalidate, 45, wc::WR_FLUSH_ERR),
                ] {
                    let result = if operation == Operation::WriteImm {
                        post_write(
                            q,
                            &source,
                            1,
                            target,
                            key + 100,
                            0,
                            POST_IMM | POST_SIGNALED,
                            id,
                        )
                    } else {
                        invalidate(q, key + 100, id)
                    };
                    assert_eq!(result, 0);
                    events(&sim, &[(operation, qpn, Some(id), 0, false)]);
                    sim.fault(Operation::Poll, Fault::Reject);
                    let mut completion = Completion::default();
                    assert_eq!(poll(cq, &mut completion, 1), -1);
                    events(&sim, &[(Operation::Poll, qpn, None, -1, false)]);
                    assert_eq!(poll(cq, &mut completion, 1), 1);
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
            stop_both(&link);
            stop_both(&other);
            assert_eq!(peek(&link.pages, 0, 17), [0xa5; 17]);
            assert_eq!(peek(&link.pages, 4096, 17), [0; 17]);
        }
        assert_eq!(sim.live_resources(), 0);
        assert_eq!(foreign.live_resources(), 0);
    }

    #[test]
    /// A dma-buf region has no CPU mapping and is never a send, write, or
    /// receive buffer, in Rust or at the adapter: only windows reach it.
    fn dmabuf_regions_reject_cpu_and_copy_paths() {
        let sim = Simulation::new();
        {
            let link = link(&sim, |_| {});
            let memory = MemfdMemory::new(8192, 0x5000_0000).unwrap();
            let (quota, _) = lifetime_tests::quota();
            let region =
                NativeRegion::dmabuf(link.pages.device.clone(), memory.clone(), quota).unwrap();
            assert!(region.is_device() && !region.is_implicit());
            assert!(region.as_ptr().is_null());
            assert_eq!(region.address(), 0x5000_0000);
            assert_eq!(region.length(), 8192);
            assert_eq!(
                link.a.send(wr(1), &region, 0, 16),
                Err(Error::InvalidRequest)
            );
            assert_eq!(
                link.a.post_recv(wr(2), &region, 0, 16),
                Err(Error::InvalidRequest)
            );
            assert_eq!(
                link.a
                    .write_imm(wr(3), &region, 0, 16, link.pages.address(), 1, 0),
                Err(Error::InvalidRequest)
            );
            sim.take_trace();
            let q = link.a.conn.qp.get().unwrap().as_ptr();
            let qpn = link.a.endpoint.qpn;
            // The adapter would see the region's key and its iova.
            let sge = Sge {
                addr: 0x5000_0000,
                length: 16,
                lkey: region.lkey,
            };
            unsafe {
                assert!(bytes(region.raw.unwrap().as_ptr()).is_null());
                assert_eq!(post_send(q, &sge, 1, 51), -1);
                events(&sim, &[(Operation::Send, qpn, Some(51), -1, false)]);
                assert_eq!(post_recv(q, &sge, 1, 52), -1);
                events(&sim, &[(Operation::PostRecv, qpn, Some(52), -1, false)]);
                let target = link.pages.address();
                assert_eq!(
                    post_write(q, &sge, 1, target, 1, 0, POST_IMM | POST_SIGNALED, 53),
                    -1
                );
                events(&sim, &[(Operation::WriteImm, qpn, Some(53), -1, false)]);
            }
            drop(region);
            assert_eq!(Arc::strong_count(&memory), 1, "deregister drops the owner");
            stop_both(&link);
        }
        assert_eq!(sim.live_resources(), 0);
    }

    /// Two connected QPs on one device with explicit depths, a page region,
    /// and a control region.
    struct Link {
        a: Rc<NativeQueuePair>,

        b: Rc<NativeQueuePair>,

        pages: Rc<NativeRegion>,

        control: Rc<NativeRegion>,
    }

    impl Link {
        /// Another unconnected QP on the link's device.
        fn qp(&self) -> Rc<NativeQueuePair> {
            NativeQueuePair::new_with_depth(
                self.b.device.clone(),
                GuardOwner::new(Arc::new(())),
                Arc::new(std::sync::atomic::AtomicBool::new(false)),
                8,
                4,
                &racer_io::Rng::os(),
            )
            .unwrap()
        }
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
                &racer_io::Rng::os(),
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
        link.a.connect(link.b.endpoint, true).unwrap();
        link.b.connect(link.a.endpoint, false).unwrap();
        link
    }

    /// Successful completions from one progress call.
    fn drain(qp: &NativeQueuePair) -> Result<Vec<WorkCompletion>> {
        let mut out = Vec::new();
        qp.progress_with(|c, _| out.push(c))?;
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
            .bind_window(id, &window, &link.pages, offset, length, u64::MAX)
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
    /// Writes outside the bound range, on another window's key, with an
    /// earlier generation of the same window, or from a QP the grant's QP is
    /// not connected to are refused without writing.
    fn write_imm_respects_bound_range_and_key_generation() {
        for case in 0..6 {
            let sim = Simulation::new();
            {
                let link = link(&sim, |_| {});
                // Connected to `b`, but `b` is not connected back to it.
                let stranger = link.qp();
                stranger.connect(link.b.endpoint, true).unwrap();
                let writer = if case == 5 { &stranger } else { &link.a };
                let (window, first) = grant(&link, wr(1), 4096, 100);
                link.b.invalidate_window(wr(2), &window, first).unwrap();
                drain(&link.b).unwrap();
                let second = link
                    .b
                    .bind_window(wr(3), &window, &link.pages, 4096, 100, u64::MAX)
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
                    4 => (base + 90, 10, second),
                    _ => (base, 10, second),
                };
                poke(&link.pages, 0, &[1; 10]);
                writer
                    .write_imm(wr(5), &link.pages, 0, length, address, key, 0)
                    .unwrap();
                if case == 4 {
                    assert_eq!(drain(&link.a).unwrap().len(), 1);
                    assert_eq!(drain(&link.b).unwrap().len(), 1);
                    assert_eq!(peek(&link.pages, 4096 + 90, 10), [1; 10]);
                } else {
                    assert_eq!(drain(writer), Err(Error::Io));
                    assert_eq!(writer.failure().unwrap().status, status::REM_ACCESS_ERR);
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
                link.b
                    .bind_window(wr(1), &window, &remote, 0, 64, u64::MAX)
                    .unwrap();
                assert_eq!(drain(&link.b).unwrap().len(), 1);
                // The ranged API refuses control regions before posting.
                let other = MemoryWindow::new(&link.b.device).unwrap();
                assert_eq!(
                    link.b
                        .bind_window(wr(2), &other, &link.control, 0, 64, u64::MAX),
                    Err(Error::InvalidRequest)
                );
                link.b
                    .bind_window(wr(3), &other, &link.pages, 0, 64, u64::MAX)
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
            let device = open(c"sim0".as_ptr(), 0);
            for access in [0, 3, 4, 8, 7] {
                assert!(register(device, 32, access).is_null());
            }
            let control = register(device, 64, 2);
            let pages = register(device, 64, 6);
            assert!(!control.is_null() && !pages.is_null());
            let sge = |region: *mut c_void, offset: u64, length: u32| Sge {
                addr: bytes(region) as u64 + offset,
                length,
                lkey: lkey(region),
            };
            let (cq_a, cq_b) = (
                cq_create(device, 8, std::ptr::null_mut()),
                cq_create(device, 8, std::ptr::null_mut()),
            );
            let mut qpn = 0;
            let a = qp(device, cq_a, 1, 4, 4, &mut qpn);
            let local = Endpoint {
                gid: [1; 16],
                qpn,
                psn: 0,
                mtu: 3,
                lid: 0,
                port: 1,
                link_layer: 2,
            };
            let b = qp(device, cq_b, 1, 4, 4, &mut qpn);
            let remote = Endpoint { qpn, ..local };
            // Receives work before connect; sends do not.
            assert_eq!(post_recv(a, &sge(control, 0, 64), 1, 1), 0);
            assert_eq!(post_recv(a, &sge(control, 1, 64), 1, 2), -1);
            assert_eq!(post_recv(a, &sge(control, 0, 0), 1, 2), -1);
            assert_eq!(post_recv(a, &sge(control, 0, 8), 2, 2), -1);
            assert_eq!(post_send(a, &sge(control, 0, 8), 1, 3), -1);
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
            assert_eq!(poll(cq_a, &mut c, 1), 1);
            assert_eq!((c.id, c.status, c.opcode), (4, 6, u32::MAX));
            // In the error state, posted work and receives flush.
            assert_eq!(post_send(a, &sge(control, 0, 8), 1, 5), 0);
            let mut out = [Completion::default(); 4];
            assert_eq!(poll(cq_a, out.as_mut_ptr(), 4), 2);
            assert_eq!(
                out[..2]
                    .iter()
                    .map(|c| (c.id, c.status, c.opcode))
                    .collect::<Vec<_>>(),
                [(5, 5, u32::MAX), (1, 5, u32::MAX)]
            );
            // A posted receive pins its region; a QP pins its CQ.
            assert_eq!(post_recv(b, &sge(control, 0, 64), 1, 6), 0);
            assert_eq!(deregister(control), -1);
            assert_eq!(cq_free(cq_b), -1);
            // A bind must use a new key generation.
            assert_eq!(bind(a, w, pages, key, 8, 0, 8), -1);
            assert_eq!(qp_free(b), 0);
            assert_eq!(qp_free(a), 0);
            assert_eq!(window_free(w), 0);
            for (raw, operation) in [
                (control, Operation::Deregister),
                (pages, Operation::Deregister),
                (cq_a, Operation::QpFree),
                (cq_b, Operation::QpFree),
                (device, Operation::Close),
            ] {
                assert_eq!(free(raw, operation), 0);
            }
        }
        assert_eq!(sim.live_resources(), 0);
    }

    #[test]
    /// A delayed write lands only when it completes, and an invalidated key
    /// rejects the next write.
    fn delayed_write_waits_for_completion_and_invalidation_revokes_key() {
        let sim = Simulation::new();
        {
            let link = link(&sim, |_| {});
            let (window, key) = grant(&link, wr(1), 4096, 17);
            let address = link.pages.address() + 4096;
            link.b.post_recv(wr(2), &link.control, 0, 64).unwrap();
            poke(&link.pages, 0, &[0xa5; 17]);
            sim.fault(Operation::WriteImm, Fault::Delay(2));
            link.a
                .write_imm(wr(3), &link.pages, 0, 17, address, key, 0)
                .unwrap();
            assert_eq!(drain(&link.a).unwrap(), []);
            assert_eq!(drain(&link.a).unwrap(), []);
            assert_eq!(peek(&link.pages, 4096, 17), [0; 17]);
            assert_eq!(drain(&link.a).unwrap().len(), 1);
            assert_eq!(peek(&link.pages, 4096, 17), [0xa5; 17]);
            assert_eq!(drain(&link.b).unwrap().len(), 1);
            link.b.invalidate_window(wr(4), &window, key).unwrap();
            assert_eq!(drain(&link.b).unwrap().len(), 1);
            // The key is revoked even before the QP stop fence.
            link.b.post_recv(wr(5), &link.control, 0, 64).unwrap();
            poke(&link.pages, 0, &[0xff; 17]);
            link.a
                .write_imm(wr(6), &link.pages, 0, 17, address, key, 0)
                .unwrap();
            assert_eq!(drain(&link.a), Err(Error::Io));
            stop_both(&link);
            assert_eq!(peek(&link.pages, 4096, 17), [0xa5; 17]);
        }
        assert_eq!(sim.live_resources(), 0);
    }

    #[test]
    /// In both completion orders of a delayed write and a delayed
    /// invalidate, a region granted again after the stop fence gets a fresh
    /// key, and the old key cannot modify it.
    fn regrant_after_fence_rejects_old_key_in_either_completion_order() {
        for remote_first in [false, true] {
            let sim = Simulation::new();
            {
                let link = link(&sim, |_| {});
                let (window, key) = grant(&link, wr(1), 4096, 17);
                let address = link.pages.address() + 4096;
                let (sender, receiver) = (link.qp(), link.qp());
                sender.connect(receiver.endpoint, true).unwrap();
                receiver.connect(sender.endpoint, false).unwrap();
                link.b.post_recv(wr(2), &link.control, 0, 64).unwrap();
                poke(&link.pages, 0, &[0xa5; 17]);
                sim.fault(Operation::WriteImm, Fault::Delay(1));
                link.a
                    .write_imm(wr(3), &link.pages, 0, 17, address, key, 0)
                    .unwrap();
                assert_eq!(drain(&link.a).unwrap(), []);
                // Revocation requested, but not complete yet.
                sim.fault(Operation::Invalidate, Fault::Delay(1));
                link.b.invalidate_window(wr(4), &window, key).unwrap();
                assert_eq!(drain(&link.b).unwrap(), []);
                if remote_first {
                    assert_eq!(drain(&link.b).unwrap().len(), 1);
                    assert_eq!(drain(&link.a), Err(Error::Io));
                } else {
                    // A queued write can land after the invalidate request.
                    assert_eq!(drain(&link.a).unwrap().len(), 1);
                    assert_eq!(drain(&link.b).unwrap().len(), 2);
                }
                link.b.stop().unwrap();
                let expected = if remote_first { [0; 17] } else { [0xa5; 17] };
                assert_eq!(peek(&link.pages, 4096, 17), expected);
                // Grant the same bytes again on a fresh pair.
                let fresh = MemoryWindow::new(&receiver.device).unwrap();
                let fresh_key = receiver
                    .bind_window(wr(5), &fresh, &link.pages, 4096, 17, u64::MAX)
                    .unwrap();
                assert_ne!(fresh_key, key);
                assert_eq!(drain(&receiver).unwrap().len(), 1);
                receiver.post_recv(wr(6), &link.control, 64, 64).unwrap();
                poke(&link.pages, 0, &[0xff; 17]);
                sender
                    .write_imm(wr(7), &link.pages, 0, 17, address, key, 0)
                    .unwrap();
                assert_eq!(drain(&sender), Err(Error::Io));
                receiver.stop().unwrap();
                assert_eq!(
                    peek(&link.pages, 4096, 17),
                    expected,
                    "old key cannot modify the regranted destination"
                );
                link.a.stop().unwrap();
            }
            assert_eq!(sim.pending_faults(), 0);
            assert_eq!(sim.live_resources(), 0);
        }
    }

    #[test]
    /// A failed invalidation and a failed stop keep the grant live, so a late
    /// write still lands, until the stop is retried.
    fn failed_invalidation_and_stop_keep_live_grant_until_retry() {
        let sim = Simulation::new();
        {
            let link = link(&sim, |_| {});
            let (window, key) = grant(&link, wr(1), 4096, 17);
            let qpn = Some(link.b.endpoint.qpn);
            sim.fault_on(Operation::Invalidate, qpn, Fault::Completion(5));
            link.b.invalidate_window(wr(2), &window, key).unwrap();
            sim.fault_on(Operation::Stop, qpn, Fault::Reject);
            assert_eq!(drain(&link.b), Err(Error::Io));
            assert!(!link.b.ready() && !link.b.stopped());
            // No new work while stopping.
            assert_eq!(
                link.b.invalidate_window(wr(3), &window, key),
                Err(Error::Unavailable)
            );
            // Late DMA: the data lands. The receiver is in the error state,
            // so the immediate finds no receive and the sender sees RNR.
            poke(&link.pages, 0, &[0xa5; 17]);
            let address = link.pages.address() + 4096;
            link.a
                .write_imm(wr(4), &link.pages, 0, 17, address, key, 0)
                .unwrap();
            assert_eq!(drain(&link.a), Err(Error::Io));
            assert_eq!(link.a.failure().unwrap().status, status::RNR_RETRY_EXC_ERR);
            // The next progress retries the stop.
            assert_eq!(drain(&link.b), Err(Error::Canceled));
            assert!(link.b.stopped());
            assert_eq!(peek(&link.pages, 4096, 17), [0xa5; 17]);
            stop_both(&link);
        }
        assert_eq!(sim.pending_faults(), 0);
        assert_eq!(sim.live_resources(), 0);
    }

    #[test]
    /// A rejected post does not hold its source; stop cancels delayed DMA.
    fn stop_cancels_delayed_dma_and_post_rejection_releases_source() {
        let sim = Simulation::new();
        {
            let link = link(&sim, |_| {});
            let (_window, key) = grant(&link, wr(1), 4096, 17);
            let address = link.pages.address() + 4096;
            link.b.post_recv(wr(2), &link.control, 0, 64).unwrap();
            poke(&link.control, 512, &[0xa5; 17]);
            let held = Rc::strong_count(&link.control);
            sim.fault(Operation::WriteImm, Fault::Reject);
            assert_eq!(
                link.a
                    .write_imm(wr(3), &link.control, 512, 17, address, key, 0),
                Err(Error::Io)
            );
            assert_eq!(Rc::strong_count(&link.control), held);
            sim.fault(Operation::WriteImm, Fault::Delay(3));
            link.a
                .write_imm(wr(3), &link.control, 512, 17, address, key, 0)
                .unwrap();
            assert_eq!(Rc::strong_count(&link.control), held + 1);
            link.a.stop().unwrap();
            assert_eq!(Rc::strong_count(&link.control), held);
            assert_eq!(drain(&link.a).unwrap(), []);
            link.b.stop().unwrap();
            assert_eq!(peek(&link.pages, 4096, 17), [0; 17]);
        }
        assert_eq!(sim.live_resources(), 0);
    }

    #[test]
    /// A poll error with a failed stop keeps the source held until a retry.
    fn polling_failure_retains_pending_source_until_stop_succeeds() {
        let sim = Simulation::new();
        {
            let link = link(&sim, |_| {});
            let (_window, key) = grant(&link, wr(1), 4096, 17);
            let held = Rc::strong_count(&link.control);
            let address = link.pages.address() + 4096;
            link.a
                .write_imm(wr(2), &link.control, 0, 17, address, key, 0)
                .unwrap();
            let qpn = Some(link.a.endpoint.qpn);
            sim.fault_on(Operation::Poll, qpn, Fault::Reject);
            sim.fault_on(Operation::Stop, qpn, Fault::Reject);
            assert_eq!(drain(&link.a), Err(Error::Io));
            assert_eq!(Rc::strong_count(&link.control), held + 1);
            link.a.stop().unwrap();
            assert_eq!(Rc::strong_count(&link.control), held);
            link.b.stop().unwrap();
            assert_eq!(peek(&link.pages, 4096, 17), [0; 17]);
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
            let region =
                NativeRegion::with_access(device, 32, Access::PAGES, lifetime_tests::quota().0)
                    .unwrap();
            let address = region.address();
            drop(scope);
            drop(region);
            assert_eq!(sim.live_resources(), 0);
            (address, sim.trace())
        }

        assert_eq!(run(), run());
    }
}
