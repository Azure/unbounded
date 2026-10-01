use super::*;
mod address;
mod completion;
mod lifecycle;

mod empty_submit_tests;
mod reserved_submission_tests;
mod socket;
use crate::{
    model::{Limits, RequestId},
    runtime::deadline::{Cancellation, Deadline},
};
use std::sync::atomic::{AtomicUsize, Ordering};
use std::{num::NonZeroUsize, os::unix::net::UnixStream, time::Instant};

#[derive(Default)]
struct Count(AtomicUsize);
impl std::task::Wake for Count {
    fn wake(self: Arc<Self>) {
        self.0.fetch_add(1, Ordering::Relaxed);
    }
}

struct Buffer(Vec<u8>, Rc<Cell<usize>>);
impl sealed::Sealed for Buffer {}
impl IoBuffer for Buffer {
    fn bytes(&self) -> Result<&[u8]> {
        Ok(&self.0)
    }
    fn bytes_mut(&mut self) -> Result<&mut [u8]> {
        Ok(&mut self.0)
    }
}
impl Drop for Buffer {
    fn drop(&mut self) {
        self.1.set(self.1.get() + 1);
    }
}
struct Lease(Rc<Cell<usize>>);
impl Drop for Lease {
    fn drop(&mut self) {
        self.0.set(self.0.get() + 1);
    }
}
fn buffer(bytes: &[u8]) -> Buffer {
    Buffer(bytes.into(), Rc::new(Cell::new(0)))
}

#[test]
fn movable_production_buffers_preserve_subrange_through_completion() {
    use crate::{
        http::connection::{BufferRange, OwnedBuffer},
        memory::pool::BufferPool,
        peer::transport::WireBuffer,
    };
    fn check<B: IoBuffer>(buffer: B) {
        let mut range = BufferRange::new(buffer, 1..2).unwrap();
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
fn limits(capacity: usize) -> Limits {
    let n = NonZeroUsize::new(1024 * 1024).unwrap();
    Limits {
        plaintext_bytes: n,
        ciphertext_bytes: n,
        dirty_bytes: n,
        registered_bytes: n,
        request_context_bytes: n,
        flights: n,
        waiters_per_flight: n,
        queue_entries: NonZeroUsize::new(capacity).unwrap(),
        connections_per_neighbor: n,
        client_connections: n,
        pipes: n,
        range_window_pages: n,
        header_bytes: n,
        cached_rankings: n,
        cached_paths: n,
        retained_snapshots: n,
        metadata_entries: n,
        relay_transfers: n,
    }
}
fn scope() -> RequestScope {
    RequestScope {
        body_deadlines: None,
        request: RequestId([0; 16]),
        deadline: Deadline(Instant::now() + Duration::from_secs(5)),
        cancellation: Cancellation::new().unwrap(),
    }
}
fn poll<T>(future: &mut Operation<'_, T>) -> Poll<Result<T>> {
    future
        .as_mut()
        .poll(&mut Context::from_waker(futures::task::noop_waker_ref()))
}
fn drive<T>(reactor: &Reactor, mut future: Operation<'_, T>) -> Result<T> {
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
fn kernel_reactor(capacity: usize) -> Option<Reactor> {
    // Skip only when the kernel lacks io_uring or policy denies ring creation.
    // Configuration, quota, opcode, and ordinary I/O errors must fail tests.
    match IoUring::new(2) {
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
