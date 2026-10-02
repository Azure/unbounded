use super::*;
use std::{
    os::unix::net::UnixStream,
    task::{Context, Poll},
    time::{Duration, Instant},
};

#[test]
fn movable_production_buffers_preserve_subrange_through_completion() {
    use crate::{
        http::connection::{BufferRange, OwnedBuffer},
        memory::pool::BufferPool,
        peer::transport::WireBuffer,
    };
    fn check<B: IoBuffer>(buffer: B)
    where
        Error: From<B::Error>,
        B::Error: std::fmt::Debug,
    {
        let mut range = BufferRange::new::<Error>(buffer, 1..2).unwrap();
        let ptr = range.bytes_mut().unwrap().as_mut_ptr();
        let finish: Box<dyn FnOnce() -> B> = Box::new(move || range.into_inner());
        // SAFETY: completion owns the fixed backing until after this write.
        unsafe { ptr.write(7) };
        let buffer = finish();
        assert_eq!(buffer.bytes().unwrap(), &[0, 7, 0]);
    }
    let admission = Rc::new(Admission::new(limits(4)));
    check(OwnedBuffer::new(&admission, 3).unwrap());
    check(WireBuffer::new(&admission, 3).unwrap());
    let pool = BufferPool::new(admission.clone());
    check(
        pool.plaintext(
            admission
                .reserve(
                    Some(&crate::model::CacheId("provenance".into())),
                    ResourceClass::Plaintext,
                    3,
                )
                .unwrap(),
            3,
        )
        .unwrap(),
    );
    let reactor = Reactor::new(admission);
    check(reactor.file_buffer(3).unwrap());
}

fn limits(capacity: usize) -> crate::model::Limits {
    let mut limits = crate::test_support::cluster::config(false).limits;
    limits.queue_entries = std::num::NonZeroUsize::new(capacity).unwrap();
    limits
}
pub(super) fn scope() -> RequestScope {
    RequestScope::new(
        crate::model::RequestId([0; 16]),
        Instant::now() + Duration::from_secs(5),
    )
    .unwrap()
}
pub(super) fn poll<T>(future: &mut Operation<'_, T>) -> Poll<Result<T>> {
    future
        .as_mut()
        .poll(&mut Context::from_waker(futures::task::noop_waker_ref()))
}
pub(super) fn drive<T>(reactor: &Reactor, mut future: Operation<'_, T>) -> Result<T> {
    let deadline = Instant::now() + Duration::from_secs(5);
    loop {
        if let Poll::Ready(result) = poll(&mut future) {
            return result;
        }
        assert!(Instant::now() < deadline, "reactor failed to make progress");
        reactor.poll_budgeted(8).unwrap();
        reactor.wait(Duration::from_millis(1)).unwrap();
    }
}
pub(super) fn kernel_reactor(capacity: usize) -> Option<Reactor> {
    match io_uring::IoUring::new(2) {
        Ok(ring) => drop(ring),
        Err(error)
            if matches!(
                error.raw_os_error(),
                Some(libc::ENOSYS | libc::EPERM | libc::EACCES)
            ) =>
        {
            eprintln!("io_uring kernel test unavailable: {error}");
            return None;
        }
        Err(error) => panic!("unexpected io_uring setup failure: {error}"),
    }
    let reactor = Reactor::new(Rc::new(Admission::new(limits(capacity))));
    reactor.init().unwrap();
    Some(reactor)
}
struct Buffer(Vec<u8>);
// SAFETY: private fixed Vec owns stable exclusive backing.
unsafe impl IoBuffer for Buffer {
    type Error = Error;
    fn bytes(&self) -> Result<&[u8]> {
        Ok(&self.0)
    }
    fn bytes_mut(&mut self) -> Result<&mut [u8]> {
        Ok(&mut self.0)
    }
}
fn buffer(bytes: &[u8]) -> Buffer {
    Buffer(bytes.into())
}
#[test]
fn file_fence_selects_only_the_matching_racer_request() {
    let sim = simulation::Simulation::new();
    let _environment = sim.enter();
    let reactor = Reactor::new(Rc::new(Admission::new(limits(4))));
    sim.write_file(std::path::Path::new("/one"), b"one")
        .unwrap();
    sim.write_file(std::path::Path::new("/two"), b"two")
        .unwrap();
    let first = scope();
    let second = RequestScope::new(crate::model::RequestId([1; 16]), first.deadline.0).unwrap();
    sim.inject("open", simulation::Fault::Delay(100));
    let mut a = reactor.file_open(
        None,
        std::ffi::CString::new("/one").unwrap(),
        libc::O_RDONLY,
        0,
        &first,
    );
    assert!(poll(&mut a).is_pending());
    sim.inject("open", simulation::Fault::Delay(100));
    let mut b = reactor.file_open(
        None,
        std::ffi::CString::new("/two").unwrap(),
        libc::O_RDONLY,
        0,
        &second,
    );
    assert!(poll(&mut b).is_pending());
    drive(&reactor, reactor.file_fence(first.request)).unwrap();
    assert_eq!(reactor.in_flight(), 1);
    assert!(matches!(poll(&mut a), Poll::Ready(Err(Error::Cancelled))));
    assert!(poll(&mut b).is_pending());
    drop((a, b));
    drive(&reactor, reactor.file_fence(second.request)).unwrap();
    assert_eq!(reactor.in_flight(), 0);
}
#[test]
fn listener_retry_preserves_racer_policy_after_capacity_recovery() {
    let sim = simulation::Simulation::new();
    let _environment = sim.enter();
    let reactor = Reactor::new(Rc::new(Admission::new(limits(1))));
    let request = scope();
    let (fd, _peer) = sim.socket_pair();
    let mut busy = reactor.readiness(Rc::new(fd), libc::POLLIN as u32, &request);
    assert!(poll(&mut busy).is_pending());
    let address = SocketAddress::Unix("/retry-listener".into());
    let listener = Rc::new(sim.listen(address.clone()).unwrap());
    let mut accept =
        crate::runtime::retry_listener(&request, || reactor.accept(listener.clone(), &request));
    assert!(poll(&mut accept).is_pending());
    assert_eq!(reactor.in_flight(), 1);
    drop(busy);
    for _ in 0..4 {
        reactor.poll_budgeted(8).unwrap();
    }
    let _client = sim.connect(address).unwrap();
    drop(drive(&reactor, accept).unwrap());
    assert_eq!(reactor.in_flight(), 0);
}
#[test]
fn real_drain_io_preserves_control_capacity_after_admission_stop() {
    let Some(reactor) = kernel_reactor(2) else {
        return;
    };
    assert_eq!(reactor.admission.used(ResourceClass::ControlProgress), 0);
    let control = reactor
        .admission
        .reserve(None, ResourceClass::ControlProgress, 2)
        .unwrap();
    reactor.admission.stop();
    let scope = scope();
    let (left, right) = UnixStream::pair().unwrap();
    let left = Rc::new(Descriptor::from(left));
    let right = Rc::new(Descriptor::from(right));
    drive(&reactor, reactor.send(left, buffer(b"drain"), (), &scope)).unwrap();
    let read = drive(
        &reactor,
        reactor.recv(right.clone(), buffer(&[0; 8]), (), &scope),
    )
    .unwrap();
    assert_eq!(&read.buffer.bytes().unwrap()[..read.bytes], b"drain");
    let mut pending = reactor.readiness(right, libc::POLLOUT as u32, &scope);
    assert!(poll(&mut pending).is_pending());
    drive(&reactor, reactor.drain()).unwrap();
    assert_eq!(reactor.in_flight(), 0);
    assert!(matches!(
        poll(&mut pending),
        Poll::Ready(Err(Error::Cancelled))
    ));
    assert_eq!(reactor.init(), Err(Error::Unavailable));
    assert_eq!(reactor.admission.used(ResourceClass::ControlProgress), 2);
    drop(control);
}

mod reserved_submission_tests {
    use super::*;
    fn reserve(admission: &Admission) -> (Reservation, Reservation) {
        (
            admission
                .reserve(None, ResourceClass::ControlProgress, 1)
                .unwrap(),
            admission
                .reserve(None, ResourceClass::RequestContext, SUBMISSION_BYTES)
                .unwrap(),
        )
    }
    #[test]
    fn provenance_capacity_and_completion_ownership() {
        let reactor = kernel_reactor(2).expect("real io_uring reserved submissions");
        let request = scope();
        let foreign = Admission::new(limits(2));
        let (slots, memory) = reserve(&foreign);
        assert!(matches!(
            reactor.reserve_submissions(slots, memory),
            Err(Error::InvalidConfiguration)
        ));
        assert_eq!(foreign.used(ResourceClass::ControlProgress), 0);
        assert_eq!(foreign.used(ResourceClass::RequestContext), 0);
        let (slots, memory) = reserve(&reactor.admission);
        let capacity = reactor.reserve_submissions(slots, memory).unwrap();
        let (slots, memory) = reserve(&reactor.admission);
        assert!(matches!(
            reactor.reserve_submissions(slots, memory),
            Err(Error::InvalidConfiguration)
        ));
        let (socket, mut peer) = UnixStream::pair().unwrap();
        let fd = Rc::new(Descriptor::from(socket));
        let mut ordinary = reactor.readiness(fd.clone(), libc::POLLIN as u32, &request);
        assert!(poll(&mut ordinary).is_pending());
        let mut overflow = reactor.readiness(fd.clone(), libc::POLLIN as u32, &request);
        assert!(matches!(
            poll(&mut overflow),
            Poll::Ready(Err(Error::Overloaded))
        ));
        let mut send = reactor.send_reserved(fd.clone(), buffer(b"x"), capacity.clone(), &request);
        assert!(poll(&mut send).is_pending());
        let deadline = Instant::now() + Duration::from_secs(2);
        while reactor.in_flight() != 1 {
            assert!(Instant::now() < deadline);
            reactor.poll_budgeted(8).unwrap();
            reactor.wait(Duration::from_millis(1)).unwrap();
        }
        // A completed but unconsumed result still owns its reserved bookkeeping.
        let mut excess =
            reactor.send_reserved(fd.clone(), buffer(b"y"), capacity.clone(), &request);
        assert!(matches!(
            poll(&mut excess),
            Poll::Ready(Err(Error::Overloaded))
        ));
        assert!(matches!(poll(&mut send), Poll::Ready(Ok(_))));
        drop(send);
        // Successful admission below asserts the previous reply released its slot.
        let mut receive = reactor.recv_reserved(fd, buffer(&[0]), capacity.clone(), &request);
        assert!(poll(&mut receive).is_pending());
        let weak = Rc::downgrade(&capacity);
        drop((receive, capacity, ordinary));
        assert!(
            weak.upgrade().is_some(),
            "abandoned I/O must retain the partition"
        );
        assert_eq!(reactor.admission.used(ResourceClass::ControlProgress), 1);
        drive(&reactor, reactor.drain()).unwrap();
        assert!(weak.upgrade().is_none());
        assert_eq!(reactor.admission.used(ResourceClass::ControlProgress), 0);
        use std::io::Read;
        let mut byte = [0];
        peer.read_exact(&mut byte).unwrap();
        assert_eq!(&byte, b"x");
        let (slots, memory) = reserve(&reactor.admission);
        assert!(matches!(
            reactor.reserve_submissions(slots, memory),
            Err(Error::Unavailable)
        ));
    }
    #[test]
    fn attachment_cannot_overcommit_existing_ordinary_entries() {
        let reactor = kernel_reactor(2).expect("real io_uring reserved attachment");
        let request = scope();
        let (socket, _peer) = UnixStream::pair().unwrap();
        let fd = Rc::new(Descriptor::from(socket));
        let mut first = reactor.readiness(fd.clone(), libc::POLLIN as u32, &request);
        let mut second = reactor.readiness(fd, libc::POLLIN as u32, &request);
        assert!(poll(&mut first).is_pending());
        assert!(poll(&mut second).is_pending());
        let (slots, memory) = reserve(&reactor.admission);
        assert!(matches!(
            reactor.reserve_submissions(slots, memory),
            Err(Error::InvalidConfiguration)
        ));
        assert_eq!(reactor.admission.used(ResourceClass::ControlProgress), 0);
        drop((first, second));
        drive(&reactor, reactor.drain()).unwrap();
    }
}
