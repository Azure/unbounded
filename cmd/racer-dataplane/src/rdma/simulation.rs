//! Connected, thread-local test fabric implementing the production native ABI.
//! Enter a node's discovery scope before constructing NativeService/WithNative.
//! Handles retain the fabric after the scope exits. No host RNIC or sysfs is used.
//! Memory and DMA stay behind backend's unsafe boundary; wire addresses are virtual.
use super::*;
use std::collections::VecDeque;

thread_local! {
    static CURRENT: RefCell<Option<Simulation>> = const { RefCell::new(None) };
}

/// One deterministic fabric shared by nodes driven on the same test thread.
/// `with_devices` changes discovery only, retaining connections to all other nodes.
#[derive(Clone)]
pub struct Simulation {
    world: Rc<RefCell<World>>,
    devices: Vec<Device>,
}

#[derive(Clone, Debug)]
pub struct Device {
    pub name: String,
    pub gid: [u8; 16],
    pub port: u8,
    pub numa_node: Option<usize>,
}

impl Device {
    pub fn new(name: impl Into<String>, gid: [u8; 16]) -> Self {
        Self {
            name: name.into(),
            gid,
            port: 1,
            numa_node: None,
        }
    }
}

/// Restore nested discovery scopes on drop. Like the native owners, this is !Send.
pub struct Environment {
    previous: Option<Simulation>,
}
impl Drop for Environment {
    fn drop(&mut self) {
        CURRENT.with(|current| *current.borrow_mut() = self.previous.take());
    }
}

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

/// Rules are consumed by the next matching call, optionally restricted to a QPN.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Fault {
    /// Reject the synchronous call without submitting work or changing resources.
    Reject,
    /// Defer a Bind/Invalidate/Write and its CQE for this many polls of its QP.
    Delay(usize),
    /// Fail a Bind/Invalidate/Write CQE without performing its effect (nonzero status).
    Completion(u32),
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct Event {
    pub sequence: u64,
    pub operation: Operation,
    pub resource: u64,
    pub work_id: Option<u64>,
    /// 0 means success; negative means synchronous rejection; positive is WC status.
    pub result: i64,
    pub completion: bool,
}

#[derive(Default)]
struct World {
    next: u32,
    resources: BTreeMap<u32, Resource>,
    faults: VecDeque<(Operation, Option<u32>, Fault)>,
    trace: Vec<Event>,
    sequence: u64,
}
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
#[derive(Clone, Copy)]
struct Grant {
    qp: u32,
    region: u32,
    length: u32,
}
struct Work {
    id: u64,
    delay: usize,
    status: u32,
    action: Action,
}
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
    fn operation(&self) -> Operation {
        match self {
            Self::Bind { .. } => Operation::Bind,
            Self::Invalidate { .. } => Operation::Invalidate,
            Self::Write { .. } => Operation::Write,
        }
    }
    fn opcode(&self) -> u32 {
        match self {
            Self::Bind { .. } => 5,
            Self::Invalidate { .. } => 6,
            Self::Write { .. } => 1,
        }
    }
}

impl Simulation {
    pub fn new() -> Self {
        Self {
            world: Rc::new(RefCell::new(World::default())),
            devices: Vec::new(),
        }
    }
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
    pub fn enter(&self) -> Environment {
        Environment {
            previous: CURRENT.with(|current| current.replace(Some(self.clone()))),
        }
    }
    pub fn fault(&self, operation: Operation, fault: Fault) {
        self.fault_on(operation, None, fault);
    }
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
    pub fn trace(&self) -> Vec<Event> {
        self.world.borrow().trace.clone()
    }
    pub fn take_trace(&self) -> Vec<Event> {
        std::mem::take(&mut self.world.borrow_mut().trace)
    }
    pub fn live_resources(&self) -> usize {
        self.world.borrow().resources.len()
    }
    pub fn pending_faults(&self) -> usize {
        self.world.borrow().faults.len()
    }
}
impl Default for Simulation {
    fn default() -> Self {
        Self::new()
    }
}

pub(crate) fn current() -> Option<Simulation> {
    CURRENT.with(|current| current.borrow().clone())
}
pub(super) fn is_api(api: &Api) -> bool {
    std::ptr::fn_addr_eq(
        api.open,
        open as unsafe extern "C" fn(*const c_char) -> *mut c_void,
    )
}
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

// ABI pointers own only stable fabric IDs. The world owns allocations; production
// Rc owners determine when native destruction is permitted, including quarantine.
struct Handle {
    sim: Simulation,
    id: u32,
}
unsafe fn handle<'a>(raw: *mut c_void) -> &'a Handle {
    unsafe { &*raw.cast::<Handle>() }
}
fn allocate(sim: &Simulation, resource: Resource) -> *mut c_void {
    let id = sim.world.borrow_mut().insert(resource);
    Box::into_raw(Box::new(Handle {
        sim: sim.clone(),
        id,
    }))
    .cast()
}
pub(super) fn numa_node(raw: NonNull<c_void>) -> Option<usize> {
    let h = unsafe { handle(raw.as_ptr()) };
    match h.sim.world.borrow().resources.get(&h.id) {
        Some(Resource::Device(d)) => d.numa_node,
        _ => None,
    }
}
pub(super) fn address(raw: NonNull<c_void>) -> u64 {
    let h = unsafe { handle(raw.as_ptr()) };
    match h.sim.world.borrow().resources.get(&h.id) {
        Some(Resource::Region { address, .. }) => *address,
        _ => 0,
    }
}
impl World {
    fn insert(&mut self, resource: Resource) -> u32 {
        self.next = self
            .next
            .checked_add(1)
            .expect("simulation resource IDs exhausted");
        assert!(self.next <= 0xffffff, "simulation QPN space exhausted");
        self.resources.insert(self.next, resource);
        self.next
    }
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
    fn fault(&mut self, operation: Operation, resource: u32) -> Option<Fault> {
        let i = self.faults.iter().position(|(op, target, _)| {
            *op == operation && target.is_none_or(|id| id == resource)
        })?;
        self.faults.remove(i).map(|(_, _, fault)| fault)
    }
    fn rejected(&mut self, operation: Operation, resource: u32) -> bool {
        let rejected = self.fault(operation, resource) == Some(Fault::Reject);
        self.record(
            operation,
            resource,
            None,
            if rejected { -1 } else { 0 },
            false,
        );
        rejected
    }
    fn device(&self, id: u32) -> Option<u32> {
        match self.resources.get(&id)? {
            Resource::Device(_) => Some(id),
            Resource::Qp { device, .. }
            | Resource::Region { device, .. }
            | Resource::Window { device, .. } => Some(*device),
        }
    }
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
    fn region_fits(&self, region: u32, length: u32) -> bool {
        matches!(self.resources.get(&region), Some(Resource::Region { bytes, .. }) if length > 0 && length as usize <= bytes.len())
    }
    fn execute(&mut self, qp: u32, action: &Action) -> u32 {
        match *action {
            Action::Bind {
                window,
                region,
                length,
            } => {
                if let Some(Resource::Window { grant, .. }) = self.resources.get_mut(&window) {
                    if grant.is_none() {
                        *grant = Some(Grant { qp, region, length });
                        return 0;
                    }
                }
            }
            Action::Invalidate { key } => {
                if let Some(Resource::Window { grant, .. }) = self.resources.get_mut(&key) {
                    if grant.is_some_and(|g| g.qp == qp) {
                        *grant = None;
                        return 0;
                    }
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

unsafe extern "C" fn discover(out: *mut Port, capacity: u32) -> c_int {
    let Some(sim) = current() else {
        return -1;
    };
    if sim.world.borrow_mut().rejected(Operation::Discover, 0) {
        return -1;
    }
    if sim.devices.len() > capacity as usize {
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
    sim.devices.len() as c_int
}
unsafe extern "C" fn open(name: *const c_char) -> *mut c_void {
    let Some(sim) = current() else {
        return std::ptr::null_mut();
    };
    if sim.world.borrow_mut().rejected(Operation::Open, 0) {
        return std::ptr::null_mut();
    }
    let name = unsafe { CStr::from_ptr(name) }.to_bytes();
    let Some(device) = sim.devices.iter().find(|d| d.name.as_bytes() == name) else {
        return std::ptr::null_mut();
    };
    allocate(&sim, Resource::Device(device.clone()))
}
unsafe extern "C" fn qp(device: *mut c_void, port: u8, _: u32, qpn: *mut u32) -> *mut c_void {
    let h = unsafe { handle(device) };
    let mut world = h.sim.world.borrow_mut();
    if world.rejected(Operation::Qp, h.id)
        || !matches!(world.resources.get(&h.id), Some(Resource::Device(d)) if d.port == port)
    {
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
        *qpn = handle(raw).id;
    }
    raw
}
unsafe extern "C" fn connect(
    raw: *mut c_void,
    local: *const Endpoint,
    remote: *const Endpoint,
) -> c_int {
    let h = unsafe { handle(raw) };
    let (local, remote) = unsafe { (*local, *remote) };
    let mut world = h.sim.world.borrow_mut();
    if world.rejected(Operation::Connect, h.id)
        || local.qpn != h.id
        || local.validate().is_err()
        || remote.validate().is_err()
    {
        return -1;
    }
    let Some(device_id) = world.device(h.id) else {
        return -1;
    };
    if !matches!(world.resources.get(&device_id), Some(Resource::Device(d)) if d.gid == local.gid && d.port == local.port)
    {
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
    if !matches!(world.resources.get(&remote_device), Some(Resource::Device(d)) if d.gid == remote.gid && d.port == remote.port)
    {
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
}
unsafe extern "C" fn stop(raw: *mut c_void) -> c_int {
    let h = unsafe { handle(raw) };
    let mut world = h.sim.world.borrow_mut();
    if world.rejected(Operation::Stop, h.id) {
        return -1;
    }
    let Some(Resource::Qp { stopped, work, .. }) = world.resources.get_mut(&h.id) else {
        return -1;
    };
    *stopped = true;
    work.clear();
    for resource in world.resources.values_mut() {
        if let Resource::Window { grant, .. } = resource {
            if grant.is_some_and(|g| g.qp == h.id) {
                *grant = None;
            }
        }
    }
    0
}
unsafe fn free(raw: *mut c_void, operation: Operation) -> c_int {
    let h = unsafe { handle(raw) };
    let mut world = h.sim.world.borrow_mut();
    if world.rejected(operation, h.id) {
        return -1;
    }
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
        || matches!(world.resources.get(&h.id), Some(Resource::Qp { work, .. }) if !work.is_empty())
    {
        return -1;
    }
    world.resources.remove(&h.id);
    drop(world);
    unsafe {
        drop(Box::from_raw(raw.cast::<Handle>()));
    }
    0
}
unsafe extern "C" fn close(raw: *mut c_void) -> c_int {
    unsafe { free(raw, Operation::Close) }
}
unsafe extern "C" fn qp_free(raw: *mut c_void) -> c_int {
    unsafe { free(raw, Operation::QpFree) }
}
unsafe extern "C" fn deregister(raw: *mut c_void) -> c_int {
    unsafe { free(raw, Operation::Deregister) }
}
unsafe extern "C" fn window_free(raw: *mut c_void) -> c_int {
    unsafe { free(raw, Operation::WindowFree) }
}
unsafe extern "C" fn register(device: *mut c_void, length: u32) -> *mut c_void {
    let h = unsafe { handle(device) };
    let mut world = h.sim.world.borrow_mut();
    if length == 0 || world.rejected(Operation::Register, h.id) {
        return std::ptr::null_mut();
    }
    let address = ((world.next as u64) + 1) << 32;
    drop(world);
    allocate(
        &h.sim,
        Resource::Region {
            device: h.id,
            bytes: vec![0; length as usize].into_boxed_slice(),
            address,
        },
    )
}
unsafe extern "C" fn bytes(raw: *mut c_void) -> *mut u8 {
    let h = unsafe { handle(raw) };
    match h.sim.world.borrow_mut().resources.get_mut(&h.id) {
        Some(Resource::Region { bytes, .. }) => bytes.as_mut_ptr(),
        _ => std::ptr::null_mut(),
    }
}
unsafe extern "C" fn window(device: *mut c_void, key: *mut u32) -> *mut c_void {
    let h = unsafe { handle(device) };
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
        *key = handle(raw).id;
    }
    raw
}
unsafe extern "C" fn bind(
    qp: *mut c_void,
    window: *mut c_void,
    region: *mut c_void,
    key: u32,
    id: u64,
    length: u32,
) -> c_int {
    let (q, w, r) = unsafe { (handle(qp), handle(window), handle(region)) };
    if !Rc::ptr_eq(&q.sim.world, &w.sim.world)
        || !Rc::ptr_eq(&q.sim.world, &r.sim.world)
        || key != w.id
    {
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
unsafe extern "C" fn invalidate(qp: *mut c_void, key: u32, id: u64) -> c_int {
    let q = unsafe { handle(qp) };
    q.sim
        .world
        .borrow_mut()
        .post(q.id, id, Action::Invalidate { key })
}
unsafe extern "C" fn write(
    qp: *mut c_void,
    region: *mut c_void,
    address: u64,
    key: u32,
    id: u64,
    length: u32,
) -> c_int {
    let (q, r) = unsafe { (handle(qp), handle(region)) };
    if !Rc::ptr_eq(&q.sim.world, &r.sim.world) {
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
unsafe extern "C" fn poll(qp: *mut c_void, out: *mut Completion, capacity: u32) -> c_int {
    let q = unsafe { handle(qp) };
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
        // A failed WR terminates successful execution on this QP. The production
        // owner consumes this CQE and establishes the stop fence before reuse.
        if status != 0 {
            break;
        }
    }
    count as c_int
}

#[cfg(test)]
mod tests {
    use super::*;

    fn connected(
        sim: &Simulation,
    ) -> (
        Rc<QueuePairHandle>,
        Rc<QueuePairHandle>,
        Rc<Region>,
        Rc<Region>,
    ) {
        let node = sim
            .with_devices(vec![Device::new("sim0", [1; 16])])
            .unwrap();
        let _scope = node.enter();
        let device = Rc::new(Verbs.discover().unwrap().remove(0));
        let sender = QueuePairHandle::new(device.clone()).unwrap();
        let receiver = QueuePairHandle::new(device.clone()).unwrap();
        sender.connect(receiver.endpoint).unwrap();
        receiver.connect(sender.endpoint).unwrap();
        let source = Region::new(device.clone(), 32, Box::new(())).unwrap();
        let target = Region::new(device, 32, Box::new(())).unwrap();
        source.resize(17).unwrap();
        target.resize(17).unwrap();
        source.copy_from(&[0xa5; 17]).unwrap();
        (sender, receiver, source, target)
    }

    #[test]
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
    fn fallback_cannot_reuse_quarantined_region_in_either_completion_order() {
        struct Probe(Rc<Cell<usize>>);
        impl Drop for Probe {
            fn drop(&mut self) {
                self.0.set(self.0.get() + 1);
            }
        }
        for remote_first in [false, true] {
            let sim = Simulation::new();
            let drops = Rc::new(Cell::new(0));
            {
                let (sender, receiver, source, _) = connected(&sim);
                let target =
                    Region::new(sender.device().clone(), 32, Box::new(Probe(drops.clone())))
                        .unwrap();
                target.resize(17).unwrap();
                let (window, bind) = receiver.bind(target.clone()).unwrap();
                assert_eq!(receiver.progress(), Ok(1));
                assert_eq!(bind.result(), Some(Ok(())));
                let fallback_sender = QueuePairHandle::new(sender.device().clone()).unwrap();
                let fallback_receiver = QueuePairHandle::new(sender.device().clone()).unwrap();
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
                assert_eq!(drops.get(), 0);
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
                assert_eq!(drops.get(), 0);
            }
            assert_eq!(drops.get(), 1);
            assert_eq!(sim.live_resources(), 0);
        }
    }

    #[test]
    fn expiry_is_inclusive_but_not_a_remote_fence_and_faults_are_ordered() {
        use crate::runtime::environment::{SimulationClock, now};
        use std::time::Duration;
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
                let stranger = QueuePairHandle::new(sender.device().clone()).unwrap();
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
        assert!(matches!(Verbs.discover(), Err(Error::Unavailable)));
        sim.fault(Operation::Open, Fault::Reject);
        assert!(Verbs.discover().unwrap().is_empty());
        assert_eq!(Verbs.discover().unwrap().len(), 1);
        assert_eq!(sim.live_resources(), 0);
    }

    #[test]
    fn discovery_scopes_restore_and_virtual_addresses_replay() {
        fn run() -> (u64, Vec<Event>) {
            let sim = Simulation::new();
            let node = sim
                .with_devices(vec![Device {
                    numa_node: Some(7),
                    ..Device::new("sim0", [1; 16])
                }])
                .unwrap();
            let scope = node.enter();
            let device = Rc::new(Verbs.discover().unwrap().remove(0));
            assert_eq!(device.numa_node(), Some(7));
            {
                let _empty = sim.enter();
                assert!(Verbs.discover().unwrap().is_empty());
            }
            assert_eq!(Verbs.discover().unwrap().len(), 1);
            let region = Region::new(device, 32, Box::new(())).unwrap();
            let address = region.address();
            drop(scope);
            drop(region);
            assert_eq!(sim.live_resources(), 0);
            (address, sim.trace())
        }
        assert_eq!(run(), run());
    }

    #[test]
    fn native_services_activate_discovered_nodes_and_copy_through_io_proxies() {
        use crate::{
            model::{identity::RequestId, limits::ResourceClass},
            rdma::{
                device::{Devices, FabricPort},
                lifecycle::{NativeService, pair},
                verbs,
            },
            runtime::{admission::Admission, deadline::RequestScope},
            topology::rails::{RailId, RailMapping},
        };
        let sim = Simulation::new();
        let scope = RequestScope::new(
            RequestId([7; 16]),
            std::time::Instant::now() + std::time::Duration::from_secs(30),
        )
        .unwrap();
        let admission = Admission::new(crate::test_support::cluster::config(true).limits);
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
            let devices = Devices::new(Rc::new(verbs::Verbs));
            devices.attach(io).unwrap();
            let mut activation = devices.activate(
                vec![RailMapping {
                    rail: RailId(0),
                    fabric: "sim".into(),
                    numa_node: None,
                }],
                vec![FabricPort {
                    fabric: "sim".into(),
                    device: "sim0".into(),
                    port: 1,
                    gid: Some([n; 16]),
                }],
                &admission,
                4096,
                &scope,
            );
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            assert!(activation.as_mut().poll(&mut cx).is_pending());
            // Discovery still selects this service's node after its scope has exited.
            native.poll_budgeted(1).unwrap();
            assert!(matches!(
                activation.as_mut().poll(&mut cx),
                Poll::Ready(Ok(_))
            ));
            drop(activation);
            let qp =
                verbs::QueuePairHandle::new(devices.select(RailId(0)).unwrap().handle).unwrap();
            nodes.push((native, devices, qp));
        }
        let (mut left, devices_left, sender) = nodes.remove(0);
        let (mut right, devices_right, receiver) = nodes.remove(0);
        sender.connect(receiver.endpoint).unwrap();
        receiver.connect(sender.endpoint).unwrap();
        left.poll_budgeted(1).unwrap();
        right.poll_budgeted(1).unwrap();
        sender.progress().unwrap();
        receiver.progress().unwrap();
        assert!(sender.ready() && receiver.ready());
        let target = verbs::Region::acquire(&receiver, 17).unwrap();
        let (window, bind) = receiver.bind(target.clone()).unwrap();
        right.poll_budgeted(1).unwrap();
        right.poll_budgeted(1).unwrap();
        assert_eq!(bind.result(), Some(Ok(())));
        let source = verbs::Region::acquire(&sender, 17).unwrap();
        source.copy_from(&[0xa5; 17]).unwrap();
        let write = sender
            .write(source.clone(), window.address.get(), window.key.get())
            .unwrap();
        left.poll_budgeted(1).unwrap();
        left.poll_budgeted(1).unwrap();
        assert_eq!(write.result(), Some(Ok(())));
        let invalidation = receiver.invalidate(window.clone()).unwrap();
        right.poll_budgeted(1).unwrap();
        right.poll_budgeted(1).unwrap();
        assert_eq!(invalidation.result(), Some(Ok(())));
        receiver.stop().unwrap();
        right.poll_budgeted(1).unwrap();
        assert_eq!(target.copy_to().unwrap(), [0xa5; 17]);
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
        assert_eq!(admission.used(ResourceClass::Registered), 0);
        assert_eq!(sim.live_resources(), 0);
    }
}
