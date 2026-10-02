//! Opt-in runtime OS simulation, enabled by Racer's dev dependency only.
pub use uring_runtime::reactor::simulation::*;

use super::Reactor;
use super::tests::{drive, poll, scope};
use crate::model::ResourceClass;
use crate::{
    error::{Error, Result},
    runtime::{admission::Admission, deadline::RequestScope},
};
use std::{cell::Cell, ffi::CString, path::Path, rc::Rc, time::Duration};
fn reactor() -> Reactor {
    Reactor::new(Rc::new(Admission::new(
        crate::test_support::cluster::config(false).limits,
    )))
}
#[test]
fn simulated_pipe_splice_preserves_suffix_under_backpressure() {
    let sim = Simulation::new();
    let _environment = sim.enter();
    let admission = Rc::new(Admission::new(
        crate::test_support::cluster::config(false).limits,
    ));
    let pool =
        crate::memory::pipe::PipePool::new(admission.clone(), Rc::new(Reactor::new(admission)));
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
    use crate::runtime::{admission::Reservation, reactor::IoBuffer};
    use page_alloc::{AlignedBuffer, Alignment};
    struct View {
        buffer: AlignedBuffer<Reservation>,
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
        let clock = crate::runtime::environment::SimulationClock::new(99);
        let _clock = clock.environment(0).enter();
        let sim = Simulation::new();
        sim.set_cancel_first(cancel_first);
        let _environment = sim.enter();
        let r = reactor();
        r.init().unwrap();
        let baseline = r.admission.used(ResourceClass::RequestContext);
        let start = crate::runtime::environment::now();
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
    use crate::model::{CacheId, CacheKey, ObjectId, ObjectVersion, StrongEtag, VersionMetadata};
    for cancel_first in [false, true] {
        let sim = Simulation::new();
        sim.set_cancel_first(cancel_first);
        let _environment = sim.enter();
        let r = reactor();
        let scope = scope();
        let bundle = crate::memory::pool::tests::bundle_for(
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
    use crate::runtime::admission::Reservation;
    use page_alloc::Slab;
    let sim = Simulation::new();
    let _environment = sim.enter();
    let r = Rc::new(reactor());
    let slabs = Slab::<Reservation>::new(
        "/slabs/worker-0-slab-0.dat".into(),
        64 * 1024 * 1024,
        32 * 1024 * 1024,
        crate::model::PAGE_BYTES as usize + crate::store::format::MAX_HEADER_BYTES + 16,
    );
    assert!(slabs.open_now().is_ok());
    let other = Slab::<Reservation>::new(
        "/slabs/worker-0-slab-0.dat".into(),
        64 * 1024 * 1024,
        32 * 1024 * 1024,
        crate::model::PAGE_BYTES as usize + crate::store::format::MAX_HEADER_BYTES + 16,
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
        Box::pin(crate::control::async_files::projected_file(
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
        Box::pin(crate::control::async_files::directory(
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
        Box::pin(crate::control::async_files::atomic_write(
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
