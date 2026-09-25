//! Faults exist only under cfg(test). Exercise the production ownership machinery.
use super::*;
#[cfg(feature = "rdma")]
use std::time::Duration;
use std::{collections::VecDeque, time::Instant};

#[derive(Default)]
struct Faults {
    events: Vec<&'static str>,
    cq: VecDeque<Completion>,
    stop_fails: bool,
    post_fails: bool,
    poll_fails: bool,
}
thread_local! { static FAULTS: RefCell<Faults> = RefCell::new(Faults::default()); }
fn event(value: &'static str) {
    FAULTS.with_borrow_mut(|f| f.events.push(value));
}
fn pointer() -> *mut c_void {
    Box::into_raw(Box::new(0u8)).cast()
}
unsafe extern "C" fn discover(_: *mut Port, _: u32) -> c_int {
    0
}
unsafe extern "C" fn open(_: *const c_char) -> *mut c_void {
    pointer()
}
unsafe extern "C" fn close(p: *mut c_void) -> c_int {
    event("pd-context");
    unsafe {
        drop(Box::from_raw(p.cast::<u8>()));
    }
    0
}
unsafe extern "C" fn qp(_: *mut c_void, _: u8, _: u32, qpn: *mut u32) -> *mut c_void {
    unsafe {
        *qpn = 7;
    }
    pointer()
}
unsafe extern "C" fn connect(_: *mut c_void, _: *const Endpoint, _: *const Endpoint) -> c_int {
    0
}
unsafe extern "C" fn stop(_: *mut c_void) -> c_int {
    event("stop");
    if FAULTS.with_borrow(|f| f.stop_fails) {
        5
    } else {
        0
    }
}
unsafe extern "C" fn qp_free(p: *mut c_void) -> c_int {
    event("cq");
    unsafe {
        drop(Box::from_raw(p.cast::<u8>()));
    }
    0
}
unsafe extern "C" fn register(_: *mut c_void, length: u32) -> *mut c_void {
    Box::into_raw(Box::new(vec![0u8; length as usize])).cast()
}
unsafe extern "C" fn deregister(p: *mut c_void) -> c_int {
    event("mr");
    unsafe {
        drop(Box::from_raw(p.cast::<Vec<u8>>()));
    }
    0
}
unsafe extern "C" fn bytes(p: *mut c_void) -> *mut u8 {
    unsafe { (&mut *p.cast::<Vec<u8>>()).as_mut_ptr() }
}
unsafe extern "C" fn window(_: *mut c_void, key: *mut u32) -> *mut c_void {
    unsafe {
        *key = 71;
    }
    pointer()
}
unsafe extern "C" fn window_free(p: *mut c_void) -> c_int {
    event("mw");
    unsafe {
        drop(Box::from_raw(p.cast::<u8>()));
    }
    0
}
unsafe extern "C" fn bind(_: *mut c_void, _: *mut c_void, _: *mut c_void, _: u32, _: u64) -> c_int {
    0
}
unsafe extern "C" fn invalidate(_: *mut c_void, _: u32, _: u64) -> c_int {
    0
}
unsafe extern "C" fn write(_: *mut c_void, _: *mut c_void, _: u64, _: u32, _: u64) -> c_int {
    if FAULTS.with_borrow(|f| f.post_fails) {
        5
    } else {
        0
    }
}
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
struct Quota(Rc<Cell<usize>>);
impl Drop for Quota {
    fn drop(&mut self) {
        self.0.set(self.0.get() - 1);
        event("quota");
    }
}
fn fixture() -> (Rc<QueuePairHandle>, Rc<Region>, Rc<Cell<usize>>) {
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
    let device = Rc::new(DeviceHandle {
        api,
        raw: NonNull::new(pointer()).unwrap(),
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
    let qp = QueuePairHandle::new(device.clone()).unwrap();
    qp.connect(qp.endpoint).unwrap();
    let charged = Rc::new(Cell::new(1));
    let region = Region::new(device, 32, Box::new(Quota(charged.clone()))).unwrap();
    (qp, region, charged)
}
fn complete(id: u64, status: u32, opcode: u32) {
    FAULTS.with_borrow_mut(|f| f.cq.push_back(Completion { id, status, opcode }));
}

#[test]
fn cancelled_waiter_retains_source_and_quota_until_terminal_fence() {
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
    FAULTS.with_borrow(|f| assert_eq!(f.events, ["stop", "mr", "quota", "cq", "pd-context"]));
}

#[test]
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
fn reversed_completions_release_only_their_own_allocation() {
    let (qp, first, first_charge) = fixture();
    let second_charge = Rc::new(Cell::new(1));
    let second = Region::new(
        qp.device.clone(),
        32,
        Box::new(Quota(second_charge.clone())),
    )
    .unwrap();
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
fn private_abi_layout_matches_c_static_assertions() {
    assert_eq!(std::mem::size_of::<Port>(), 88);
    assert_eq!(std::mem::offset_of!(Port, mtu), 80);
    assert_eq!(std::mem::size_of::<Endpoint>(), 32);
    assert_eq!(std::mem::offset_of!(Endpoint, qpn), 16);
    assert_eq!(std::mem::size_of::<Completion>(), 16);
}

#[cfg(feature = "rdma")]
#[test]
#[ignore = "requires the installed native adapter and a host with zero usable RDMA ports"]
fn native_no_device() {
    let devices = Verbs
        .discover()
        .expect("native adapter must be installed for this test");
    assert!(devices.is_empty(), "this is not a no-device host");
}

#[cfg(feature = "rdma")]
#[test]
#[ignore = "requires an active type-2B provider; exercises real loopback RC DMA"]
fn native_available_provider_write_bind_invalidate_and_fence() {
    let devices = Verbs.discover().expect("native adapter unavailable");
    let name = std::env::var("RACER_RDMA_TEST_DEVICE").expect("explicitly select a test device");
    let device = Rc::new(
        devices
            .into_iter()
            .find(|d| d.name == name)
            .expect("selected provider not active or no type-2B support"),
    );
    let receiver = QueuePairHandle::new(device.clone()).unwrap();
    let sender = QueuePairHandle::new(device.clone()).unwrap();
    receiver.connect(sender.endpoint).unwrap();
    sender.connect(receiver.endpoint).unwrap();
    let destination = Region::new(device.clone(), 4096, Box::new(())).unwrap();
    let source = Region::new(device.clone(), 4096, Box::new(())).unwrap();
    source.copy_from(&vec![0xa5; 4096]).unwrap();
    let (window, bind) = receiver.bind(destination.clone()).unwrap();
    fn wait(qp: &QueuePairHandle, ticket: &Ticket) {
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
    let stale = Region::new(device, 4096, Box::new(())).unwrap();
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
