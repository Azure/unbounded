//! Racer admission and request policy around the worker-local runtime reactor.
//!
//! Inline storage cannot meet the stable-buffer contract:
//! ```compile_fail
//! use racer_dataplane::error::Result;
//! use uring_runtime::reactor::IoBuffer;
//! struct Inline([u8; 16]);
//! impl IoBuffer for Inline {
//!     type Error = racer_dataplane::error::Error;
//!     fn bytes(&self) -> Result<&[u8]> { Ok(&self.0) }
//!     fn bytes_mut(&mut self) -> Result<&mut [u8]> { Ok(&mut self.0) }
//! }
//! ```
//! Borrowed storage does not have an independent completion lifetime:
//! ```compile_fail
//! use racer_dataplane::error::Result;
//! use uring_runtime::reactor::IoBuffer;
//! struct Borrowed<'a>(&'a mut [u8]);
//! unsafe impl IoBuffer for Borrowed<'_> {
//!     type Error = racer_dataplane::error::Error;
//!     fn bytes(&self) -> Result<&[u8]> { Ok(self.0) }
//!     fn bytes_mut(&mut self) -> Result<&mut [u8]> { Ok(self.0) }
//! }
//! ```
//! Audited production buffers own independent storage:
//! ```
//! use racer_dataplane::{memory::PlaintextBuffer,
//!     runtime::admission::AdmissionPolicy};
//! use uring_runtime::reactor::IoBuffer;
//! use flow_control::Charge;
//! use page_alloc::AlignedBuffer;
//! fn independent<T: 'static>() {}
//! fn completion_safe<B: IoBuffer>() { independent::<B>(); }
//! completion_safe::<PlaintextBuffer>();
//! completion_safe::<AlignedBuffer<Charge<AdmissionPolicy>>>();
//! ```
//! Immutable ciphertext cannot be used for receive:
//! ```compile_fail
//! use std::rc::Rc;
//! use racer_dataplane::{memory::CiphertextPage,
//!     runtime::{reactor::Reactor, deadline::RequestScope}};
//! use uring_runtime::reactor::Descriptor;
//! fn receive(r: &Reactor, fd: Rc<Descriptor>, page: CiphertextPage, scope: &RequestScope) {
//!     let _ = r.recv(fd, page, (), scope);
//! }
//! ```
use super::admission::AdmissionExt;
use super::admission::AdmissionPolicy;
use super::admission::ConnectionReservation;
use super::deadline::RequestScope;
use crate::error::Error;
use crate::error::Operation;
use crate::error::Result;
use crate::model::RequestId;
use crate::model::ResourceClass;
use std::ops::Deref;
use std::rc::Rc;
use uring_runtime::reactor::ReactorWake;
use uring_runtime::reactor::SUBMISSION_BYTES;
use uring_runtime::reactor::SubmissionCapacity;
pub mod filesystem {
    //! Racer filesystem buffer error boundary.
    use crate::error::Error;
    use crate::error::Result;
    use uring_runtime::reactor::IoBuffer;
    pub struct Buffer(pub(super) uring_runtime::reactor::filesystem::Buffer);
    // SAFETY: the runtime owner retains its private stable allocation and charge.
    unsafe impl IoBuffer for Buffer {
        type Error = Error;
        fn bytes(&self) -> Result<&[u8]> {
            self.0.bytes().map_err(Into::into)
        }
        fn bytes_mut(&mut self) -> Result<&mut [u8]> {
            self.0.bytes_mut().map_err(Into::into)
        }
    }
    impl Buffer {
        pub fn advance(&mut self, n: usize) -> Result<()> {
            self.0.advance(n).map_err(Into::into)
        }
        pub fn remaining(&self) -> usize {
            self.0.remaining()
        }
        pub fn prefix(&self, n: usize) -> Result<&[u8]> {
            self.0.prefix(n).map_err(Into::into)
        }
    }
}

pub struct AdmissionBudget(Rc<flow_control::Quotas<AdmissionPolicy>>);
impl uring_runtime::Budget for AdmissionBudget {
    type Charge = flow_control::Charge<AdmissionPolicy>;
    fn charge(&self, bytes: usize) -> uring_runtime::Result<flow_control::Charge<AdmissionPolicy>> {
        self.0
            .reserve_completion(None, ResourceClass::RequestContext, bytes)
            .map_err(|error| match error {
                flow_control::Error::InvalidInput => uring_runtime::Error::InvalidConfiguration,
                flow_control::Error::Unavailable => uring_runtime::Error::Unavailable,
                _ => uring_runtime::Error::Overloaded,
            })
    }
}

pub struct Reactor {
    core: uring_runtime::reactor::Reactor<RequestScope, AdmissionBudget>,
    admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
}
impl Deref for Reactor {
    type Target = uring_runtime::reactor::Reactor<RequestScope, AdmissionBudget>;
    fn deref(&self) -> &Self::Target {
        &self.core
    }
}
impl Reactor {
    pub fn new(admission: Rc<flow_control::Quotas<AdmissionPolicy>>) -> Self {
        Self {
            core: uring_runtime::reactor::Reactor::new(
                admission.limits().queue_entries.get(),
                AdmissionBudget(admission.clone()),
            ),
            admission,
        }
    }
    pub fn init(&self) -> Result<()> {
        self.core.init().map_err(Into::into)
    }
    pub fn poll_budgeted(&self, budget: usize) -> Result<usize> {
        self.core.poll_budgeted(budget).map_err(Into::into)
    }
    pub fn wait(&self, duration: std::time::Duration) -> Result<()> {
        self.core.wait(duration).map_err(Into::into)
    }
    pub fn waker(&self) -> Result<ReactorWake> {
        self.core.waker().map_err(Into::into)
    }
    pub fn file_buffer(&self, length: usize) -> Result<filesystem::Buffer> {
        if length == 0 {
            return Err(Error::InvalidConfiguration);
        }
        self.core
            .file_buffer(length)
            .map(filesystem::Buffer)
            .map_err(Into::into)
    }
    pub fn file_bytes(&self, bytes: &[u8]) -> Result<filesystem::Buffer> {
        if bytes.is_empty() {
            return Err(Error::InvalidConfiguration);
        }
        self.core
            .file_bytes(bytes)
            .map(filesystem::Buffer)
            .map_err(Into::into)
    }
    pub fn reserve_connection(&self, role: ResourceClass) -> Result<ConnectionReservation> {
        self.admission.reserve_connection(role)
    }
    pub(crate) fn reserve_submissions(
        &self,
        slots: flow_control::Charge<AdmissionPolicy>,
        memory: flow_control::Charge<AdmissionPolicy>,
    ) -> Result<Rc<SubmissionCapacity>> {
        let capacity = slots.amount();
        slots.validate(ResourceClass::ControlProgress, capacity)?;
        memory.validate(
            ResourceClass::RequestContext,
            capacity
                .checked_mul(SUBMISSION_BYTES)
                .ok_or(Error::InvalidConfiguration)?,
        )?;
        if !self.admission.owns(&slots) || !self.admission.owns(&memory) {
            return Err(Error::InvalidConfiguration);
        }
        self.core
            .reserve_submissions(capacity, (slots, memory))
            .map_err(|error| match error {
                uring_runtime::Error::InvalidInput => Error::InvalidConfiguration,
                other => other.into(),
            })
    }
    pub fn file_fence(&self, request: RequestId) -> Operation<'_, ()> {
        self.core
            .fence_matching(move |scope| scope.request == request)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::os::unix::net::UnixStream;
    use std::task::Context;
    use std::task::Poll;
    use std::time::Duration;
    use std::time::Instant;
    use uring_runtime::reactor::Descriptor;
    use uring_runtime::reactor::IoBuffer;
    use uring_runtime::reactor::SocketAddress;
    use uring_runtime::reactor::simulation;

    #[test]
    fn movable_production_buffers_preserve_subrange_through_completion() {
        use crate::http::OwnedBuffer;
        use crate::memory::BufferPool;
        use crate::peer::transport::WireBuffer;
        use http1::connection::BufferRange;
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
        let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(limits(4))));
        check(OwnedBuffer::new(&crate::http::HttpContext(admission.clone()), 3).unwrap());
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
        let reactor = Reactor::new(Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
            limits(capacity),
        ))));
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
        let reactor = Reactor::new(Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
            limits(4),
        ))));
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
        let reactor = Reactor::new(Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
            limits(1),
        ))));
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
        fn reserve(
            admission: &flow_control::Quotas<AdmissionPolicy>,
        ) -> (
            flow_control::Charge<AdmissionPolicy>,
            flow_control::Charge<AdmissionPolicy>,
        ) {
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
            let foreign = flow_control::Quotas::new(AdmissionPolicy::new(limits(2)));
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
            let mut send =
                reactor.send_reserved(fd.clone(), buffer(b"x"), capacity.clone(), &request);
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
}

#[cfg(test)]
mod simulation_tests {
    use super::Reactor;
    use super::tests::drive;
    use super::tests::poll;
    use super::tests::scope;
    use crate::error::Error;
    use crate::error::Result;
    use crate::model::ResourceClass;
    use crate::runtime::admission::AdmissionPolicy;
    use crate::runtime::deadline::RequestScope;
    use std::cell::Cell;
    use std::ffi::CString;
    use std::path::Path;
    use std::rc::Rc;
    use std::time::Duration;
    use uring_runtime::reactor::simulation::DiskState;
    use uring_runtime::reactor::simulation::Environment;
    use uring_runtime::reactor::simulation::Fault;
    use uring_runtime::reactor::simulation::Simulation;
    fn reactor() -> Reactor {
        Reactor::new(Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
            crate::test_support::cluster::config(false).limits,
        ))))
    }
    #[test]
    fn simulated_pipe_splice_preserves_suffix_under_backpressure() {
        let sim = Simulation::new();
        let _environment = sim.enter();
        let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
            crate::test_support::cluster::config(false).limits,
        )));
        let pool = crate::memory::new_pipe_pool(admission);
        let mut pipe = pool.acquire().unwrap();
        let (a, b) = sim.socket_pair();
        sim.set_stream_capacity(2);
        pipe.try_write(b"abc").unwrap();
        assert_eq!(pipe.try_splice_descriptor(&a, 3).unwrap(), 2);
        assert_eq!(pipe.buffered(), 1);
        assert_eq!(
            pipe.try_splice_descriptor(&a, 3).unwrap_err().kind(),
            std::io::ErrorKind::WouldBlock
        );
        let b = b.into_sim().unwrap();
        let mut bytes = [0; 2];
        b.recv(&mut bytes).unwrap();
        assert_eq!(&bytes, b"ab");
        assert_eq!(pipe.try_splice_descriptor(&a, 3).unwrap(), 1);
        assert_eq!(b.recv(&mut bytes).unwrap(), 1);
        assert_eq!(bytes[0], b'c');
    }
    #[test]
    fn direct_io_faults_check_address_offset_and_length_independently() {
        use page_alloc::AlignedBuffer;
        use page_alloc::Alignment;
        use uring_runtime::reactor::IoBuffer;
        struct View {
            buffer: AlignedBuffer<flow_control::Charge<AdmissionPolicy>>,
            start: usize,
            length: usize,
        }
        // SAFETY: fixed range retains independently allocated aligned storage.
        unsafe impl IoBuffer for View {
            type Error = Error;
            fn bytes(&self) -> Result<&[u8]> {
                Ok(&self.buffer.bytes()?[self.start..self.start + self.length])
            }
            fn bytes_mut(&mut self) -> Result<&mut [u8]> {
                Ok(&mut self.buffer.bytes_mut()?[self.start..self.start + self.length])
            }
        }
        let (sim, _environment, r, scope) = setup();
        assert_eq!(
            Alignment::new(0, 4096, 4096).map_err(Error::from),
            Err(Error::DirectIoUnsupported)
        );
        let alignment = Alignment::new(4096, 4096, 4096).unwrap();
        let path = Path::new("/direct");
        let fd = Rc::new(
            sim.open(None, path, libc::O_CREAT | libc::O_RDWR | libc::O_DIRECT)
                .unwrap(),
        );
        for (offset, start, length) in [(1, 0, 4096), (0, 1, 4096), (0, 0, 4095), (4096, 0, 4096)] {
            let quota = r
                .admission
                .reserve(None, ResourceClass::Ciphertext, 8192)
                .unwrap();
            let mut buffer = alignment.allocate(8192, quota).unwrap();
            buffer.bytes_mut().unwrap().fill(7);
            let result = drive(
                &r,
                r.write_at(
                    fd.clone(),
                    offset,
                    View {
                        buffer,
                        start,
                        length,
                    },
                    (),
                    &scope,
                ),
            );
            if offset == 4096 {
                assert_eq!(result.unwrap().bytes, 4096);
            } else {
                assert!(matches!(result, Err(Error::Io)));
                assert_eq!(
                    drive(&r, r.file_stat(fd.clone(), &scope)).unwrap().stx_size,
                    0
                );
            }
            assert_eq!(r.admission.used(ResourceClass::Ciphertext), 0);
            assert_eq!(r.in_flight(), 0);
        }
        drive(&r, r.file_sync(fd.clone(), &scope)).unwrap();
        sim.disk().sync(Path::new("/")).unwrap();
        drop(fd);
        sim.disk().crash().unwrap();
        let result = drive(
            &r,
            r.read_at(
                Rc::new(sim.open(None, path, libc::O_RDWR).unwrap()),
                4096,
                r.file_buffer(4096).unwrap(),
                (),
                &scope,
            ),
        )
        .unwrap();
        assert_eq!(result.bytes, 4096);
        assert_eq!(result.buffer.prefix(4096).unwrap(), &[7; 4096]);
    }
    fn setup() -> (Simulation, Environment, Reactor, RequestScope) {
        let sim = Simulation::new();
        let environment = sim.enter();
        (sim, environment, reactor(), scope())
    }
    struct Probe(Rc<Cell<usize>>);
    impl Drop for Probe {
        fn drop(&mut self) {
            self.0.set(self.0.get() + 1);
        }
    }
    #[test]
    fn candidate_total_cancels_pending_receive_without_bytes_and_retains_both_fences() {
        for cancel_first in [false, true] {
            let clock = uring_runtime::environment::SimulationClock::new(99);
            let _clock = clock.environment(0).enter();
            let sim = Simulation::new();
            sim.set_cancel_first(cancel_first);
            let _environment = sim.enter();
            let r = reactor();
            r.init().unwrap();
            let baseline = r.admission.used(ResourceClass::RequestContext);
            let start = uring_runtime::environment::now();
            let request = RequestScope::new(
                crate::model::RequestId([99; 16]),
                start + Duration::from_secs(60),
            )
            .unwrap();
            request
                .set_candidate_total(start + Duration::from_secs(3))
                .unwrap();
            request.set_candidate_idle(Duration::from_secs(10)).unwrap();
            let (fd, peer) = sim.socket_pair();
            let fd = Rc::new(fd);
            let weak = Rc::downgrade(&fd);
            let drops = Rc::new(Cell::new(0));
            let mut recv = r.recv(
                fd,
                r.file_buffer(8).unwrap(),
                Probe(drops.clone()),
                &request,
            );
            assert!(poll(&mut recv).is_pending());
            clock.advance(Duration::from_secs(2));
            request.candidate_progress().unwrap();
            assert_eq!(r.poll_budgeted(1), Ok(0));
            clock.advance(Duration::from_secs(1));
            assert_eq!(r.poll_budgeted(1), Ok(0));
            assert!(!request.cancellation.is_cancelled());
            assert_eq!(request.deadline.0, start + Duration::from_secs(60));
            assert_eq!(r.poll_budgeted(1), Ok(1));
            assert!(poll(&mut recv).is_pending());
            assert_eq!(drops.get(), 0);
            assert!(weak.upgrade().is_some());
            assert_eq!(r.in_flight(), 1);
            assert!(r.admission.used(ResourceClass::RequestContext) > baseline);
            assert_eq!(r.poll_budgeted(1), Ok(1));
            assert!(matches!(
                poll(&mut recv),
                std::task::Poll::Ready(Err(Error::DeadlineExceeded))
            ));
            drop(recv);
            assert_eq!(drops.get(), 1);
            assert!(weak.upgrade().is_none());
            assert_eq!(r.in_flight(), 0);
            assert_eq!(r.admission.used(ResourceClass::RequestContext), baseline);
            drop(peer);
            assert_eq!(sim.live_handles(), 0);
        }
    }
    #[test]
    fn immutable_ciphertext_send_shares_backing_and_retains_it_through_cancel_fences() {
        use crate::model::CacheId;
        use crate::model::CacheKey;
        use crate::model::ObjectId;
        use crate::model::ObjectVersion;
        use crate::model::StrongEtag;
        use crate::model::VersionMetadata;
        for cancel_first in [false, true] {
            let sim = Simulation::new();
            sim.set_cancel_first(cancel_first);
            let _environment = sim.enter();
            let r = reactor();
            let scope = scope();
            let bundle = crate::memory::tests::bundle_for(
                &r.admission,
                VersionMetadata {
                    content_type: None,
                    version: ObjectVersion {
                        object: ObjectId {
                            cache: CacheId("cache".into()),
                            key: CacheKey([0; 32]),
                        },
                        etag: StrongEtag::test_value("v1"),
                    },
                    length: 3,
                },
            );
            let page = bundle.ciphertext.clone();
            drop(bundle);
            let weak = std::sync::Arc::downgrade(&page.inner);
            let pointer = page.bytes().as_ptr();
            let (fd, peer) = sim.socket_pair();
            let fd = Rc::new(fd);
            sim.set_max_chunk(3);
            let completed = drive(&r, r.send(fd.clone(), page.clone(), (), &scope)).unwrap();
            assert_eq!(completed.bytes, 3);
            assert_eq!(completed.buffer.bytes().as_ptr(), pointer);
            assert_eq!(page.bytes(), &[2; 19]);
            assert_eq!(r.admission.used(ResourceClass::Ciphertext), 19);
            let mut received = [0; 3];
            assert_eq!(peer.try_recv(&mut received).unwrap(), 3);
            assert_eq!(received, [2; 3]);
            drop(completed);
            sim.inject("send", Fault::Delay(20));
            let mut send = r.send(fd, page, (), &scope);
            assert!(poll(&mut send).is_pending());
            drop(send);
            assert_eq!(r.poll_budgeted(1), Ok(0));
            assert_eq!(r.poll_budgeted(1), Ok(1));
            assert!(weak.upgrade().is_some());
            assert_eq!(r.admission.used(ResourceClass::Ciphertext), 19);
            assert_eq!(r.poll_budgeted(1), Ok(1));
            assert!(weak.upgrade().is_none());
            assert_eq!(r.admission.used(ResourceClass::Ciphertext), 0);
        }
    }
    #[test]
    fn real_slab_open_is_sparse_exclusive_and_checks_direct_geometry() {
        use page_alloc::Slab;
        let sim = Simulation::new();
        let _environment = sim.enter();
        let r = Rc::new(reactor());
        let slabs = Slab::<flow_control::Charge<AdmissionPolicy>>::new(
            "/slabs/worker-0-slab-0.dat".into(),
            64 * 1024 * 1024,
            32 * 1024 * 1024,
            crate::model::PAGE_BYTES as usize + crate::store::MAX_HEADER_BYTES + 16,
        );
        assert!(slabs.open_now().is_ok());
        let other = Slab::<flow_control::Charge<AdmissionPolicy>>::new(
            "/slabs/worker-0-slab-0.dat".into(),
            64 * 1024 * 1024,
            32 * 1024 * 1024,
            crate::model::PAGE_BYTES as usize + crate::store::MAX_HEADER_BYTES + 16,
        );
        assert_eq!(
            other.open_now().map_err(Error::from),
            Err(Error::Unavailable)
        );
        assert_eq!(
            other.open_now().map_err(Error::from),
            Err(Error::Unavailable)
        );
        drop(slabs);
        assert!(other.open_now().is_ok());
        let path = Path::new("/slabs/worker-0-slab-0.dat");
        let file = Rc::new(sim.open(None, path, libc::O_RDWR | libc::O_DIRECT).unwrap());
        let scope = scope();
        assert!(matches!(
            drive(
                &r,
                r.write_at(file, 1, r.file_bytes(b"bad").unwrap(), (), &scope)
            ),
            Err(Error::Io)
        ));
        let file = sim.open(None, path, libc::O_RDONLY).unwrap();
        assert_eq!(
            file.as_sim().unwrap().stat().unwrap().stx_size,
            64 * 1024 * 1024
        );
        assert_eq!(
            sim.disk().read(path, 0, 4096, DiskState::Volatile).unwrap(),
            vec![0; 4096]
        );
    }
    #[test]
    fn projected_directory_rotation_and_private_atomic_writes_use_real_filesystem_calls() {
        let (sim, _environment, r, scope) = setup();
        sim.write_file(Path::new("/projected/epoch-a/bundle"), b"first")
            .unwrap();
        sim.symlink(Path::new("epoch-a"), Path::new("/projected/..data"))
            .unwrap();
        let bytes = drive(
            &r,
            Box::pin(crate::control::projected_file(
                &r,
                Path::new("/projected"),
                "bundle",
                64,
                &scope,
            )),
        )
        .unwrap();
        assert_eq!(&**bytes, b"first");
        let private = drive(
            &r,
            Box::pin(crate::control::directory(
                &r,
                Path::new("/private"),
                true,
                true,
                &scope,
            )),
        )
        .unwrap();
        sim.inject("write", Fault::Short(2));
        drive(
            &r,
            Box::pin(crate::control::atomic_write(
                &r, &private, "identity", b"secret", &scope,
            )),
        )
        .unwrap();
        assert_eq!(
            sim.read_file(Path::new("/private/identity")).unwrap(),
            b"secret"
        );
        sim.symlink(Path::new("/private"), Path::new("/projected/escape"))
            .unwrap();
        assert!(
            drive(
                &r,
                r.file_open(
                    None,
                    CString::new("/projected/escape").unwrap(),
                    libc::O_RDONLY,
                    4,
                    &scope
                )
            )
            .is_err()
        );
    }
}
