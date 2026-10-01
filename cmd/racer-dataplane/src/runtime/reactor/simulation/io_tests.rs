//! Assertions migrated from test_support::io onto production Entry ownership.
use super::*;
use crate::{
    error::{Error, Operation, Result},
    model::{RequestId, ResourceClass},
    runtime::{admission::Admission, deadline::RequestScope, reactor::Reactor},
};
use std::{
    cell::Cell,
    task::{Context, Poll},
    time::{Duration, Instant},
};

pub(super) fn reactor() -> Reactor {
    Reactor::new(Rc::new(Admission::new(
        crate::test_support::cluster::config(false).limits,
    )))
}
pub(super) fn scope() -> RequestScope {
    RequestScope::new(RequestId([4; 16]), Instant::now() + Duration::from_secs(30)).unwrap()
}
pub(super) fn poll<T>(op: &mut Operation<'_, T>) -> Poll<Result<T>> {
    op.as_mut()
        .poll(&mut Context::from_waker(futures::task::noop_waker_ref()))
}
pub(super) fn drive<T>(r: &Reactor, mut op: Operation<'_, T>) -> Result<T> {
    for _ in 0..1000 {
        if let Poll::Ready(result) = poll(&mut op) {
            return result;
        }
        r.poll_budgeted(8)?;
        r.wait(Duration::ZERO)?;
    }
    panic!("simulation did not progress")
}

#[test]
fn real_reactor_stream_backpressure_eof_and_completion_fences() {
    let sim = Simulation::new();
    let _environment = sim.enter();
    let r = reactor();
    r.init().unwrap();
    assert!(r.state.borrow().ring.is_none());
    assert!(r.state.borrow().wake.is_none());
    let baseline = r.admission.used(ResourceClass::RequestContext);
    let scope = scope();
    let address = SocketAddress::Unix("/stream".into());
    let listener = Rc::new(sim.listen(address.clone()).unwrap());
    let client = Rc::new(Descriptor::socket(libc::AF_UNIX).unwrap());
    drive(&r, r.connect(client.clone(), address, &scope)).unwrap();
    let server = Rc::new(drive(&r, r.accept(listener.clone(), &scope)).unwrap());
    sim.set_stream_capacity(3);
    let sent = drive(
        &r,
        r.send(client.clone(), r.file_bytes(b"abcdef").unwrap(), (), &scope),
    )
    .unwrap();
    assert_eq!(sent.bytes, 3);
    drop(sent);
    let mut writable = r.readiness(client.clone(), libc::POLLOUT as u32, &scope);
    assert!(poll(&mut writable).is_pending());
    r.poll_budgeted(8).unwrap();
    assert!(poll(&mut writable).is_pending());
    let read = drive(
        &r,
        r.recv(server.clone(), r.file_buffer(8).unwrap(), (), &scope),
    )
    .unwrap();
    assert_eq!(read.bytes, 3);
    assert_eq!(read.buffer.prefix(3).unwrap(), b"abc");
    drop(read);
    assert_eq!(drive(&r, writable).unwrap(), libc::POLLOUT as u32);
    drop(client);
    assert_eq!(
        drive(
            &r,
            r.recv(server.clone(), r.file_buffer(8).unwrap(), (), &scope)
        )
        .unwrap()
        .bytes,
        0
    );
    drop((server, listener));
    assert_eq!(sim.live_handles(), 0);
    assert_eq!(r.admission.used(ResourceClass::RequestContext), baseline);
}

#[test]
fn scoped_selection_and_listener_pending_close_are_isolated() {
    let sim = Simulation::new();
    let other = Simulation::new();
    assert!(Simulation::current().is_none());
    {
        let _scope = sim.enter();
        {
            let _nested = other.enter();
            assert!(Rc::ptr_eq(&Simulation::current().unwrap().0, &other.0));
        }
        assert!(Rc::ptr_eq(&Simulation::current().unwrap().0, &sim.0));
    }
    assert!(Simulation::current().is_none());
    let address = SocketAddress::Inet("127.0.0.1:1234".parse().unwrap());
    let listener = sim.listen(address.clone()).unwrap();
    let client = sim.connect(address).unwrap();
    assert_eq!(sim.live_handles(), 3);
    drop(listener);
    assert_eq!(sim.live_handles(), 1);
    let Descriptor::Sim(client) = client else {
        unreachable!()
    };
    assert_eq!(
        client.send(b"x").unwrap_err().raw_os_error(),
        Some(libc::EPIPE)
    );
    drop(client);
    assert_eq!(sim.live_handles(), 0);
}

#[test]
fn wrapped_stream_and_pipe_copies_preserve_short_io_and_errors() {
    let sim = Simulation::new();
    let (Descriptor::Sim(writer), Descriptor::Sim(reader)) = sim.socket_pair() else {
        unreachable!()
    };
    let (Descriptor::Sim(pipe_reader), Descriptor::Sim(pipe_writer)) = sim.pipe(16) else {
        unreachable!()
    };
    // Install wrapped queues explicitly so this does not depend on allocator growth.
    let wrapped = || {
        let mut queue = VecDeque::with_capacity(16);
        queue.extend(0..16);
        queue.drain(..12);
        queue.extend(16..24);
        assert!(!queue.as_slices().1.is_empty());
        queue
    };
    {
        let mut world = sim.0.borrow_mut();
        let Resource::Socket { bytes, .. } = world.resources.get_mut(&reader.id).unwrap() else {
            unreachable!()
        };
        *bytes = wrapped();
        let Resource::Pipe { bytes, .. } = world.resources.get(&pipe_reader.id).unwrap() else {
            unreachable!()
        };
        *bytes.borrow_mut() = wrapped();
    }
    let mut output = [0xcc; 16];
    sim.inject("recv", Fault::Short(7));
    assert_eq!(reader.recv(&mut output).unwrap(), 7);
    assert_eq!(&output[..7], &[12, 13, 14, 15, 16, 17, 18]);
    assert_eq!(&output[7..], &[0xcc; 9]);
    assert_eq!(reader.recv(&mut output).unwrap(), 5);
    assert_eq!(&output[..5], &[19, 20, 21, 22, 23]);
    sim.inject("pipe_read", Fault::Short(7));
    assert_eq!(pipe_reader.pipe_read(&mut output).unwrap(), 7);
    assert_eq!(&output[..7], &[12, 13, 14, 15, 16, 17, 18]);
    assert_eq!(pipe_writer.pipe_write(&[24, 25, 26, 27]).unwrap(), 4);
    sim.inject("send", Fault::Errno(libc::EPIPE));
    assert_eq!(
        pipe_reader.splice(&writer, 9).unwrap_err().raw_os_error(),
        Some(libc::EPIPE)
    );
    sim.inject("splice", Fault::Short(6));
    assert_eq!(pipe_reader.splice(&writer, 9).unwrap(), 6);
    assert_eq!(reader.recv(&mut output).unwrap(), 6);
    assert_eq!(&output[..6], &[19, 20, 21, 22, 23, 24]);
    assert_eq!(pipe_reader.pipe_read(&mut output).unwrap(), 3);
    assert_eq!(&output[..3], &[25, 26, 27]);
    assert_eq!(
        pipe_reader.pipe_read(&mut output).unwrap_err().kind(),
        io::ErrorKind::WouldBlock
    );
    assert_eq!(
        reader.recv(&mut output).unwrap_err().kind(),
        io::ErrorKind::WouldBlock
    );
    drop(writer);
    assert_eq!(reader.recv(&mut output).unwrap(), 0);
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
        io::ErrorKind::WouldBlock
    );
    let Descriptor::Sim(b) = b else {
        unreachable!()
    };
    let mut bytes = [0; 2];
    b.recv(&mut bytes).unwrap();
    assert_eq!(&bytes, b"ab");
    assert_eq!(pipe.try_splice_descriptor(&a, 3).unwrap(), 1);
    assert_eq!(b.recv(&mut bytes).unwrap(), 1);
    assert_eq!(bytes[0], b'c');
}

#[test]
fn datagrams_preserve_packet_boundaries_and_source_addresses() {
    let sim = Simulation::new();
    let server_address = "127.0.0.1:53".parse().unwrap();
    let server = sim.bind_datagram(server_address).unwrap();
    let client = sim.bind_datagram("127.0.0.1:0".parse().unwrap()).unwrap();
    let (Descriptor::Sim(server), Descriptor::Sim(client)) = (server, client) else {
        unreachable!()
    };
    client.connect_datagram(server_address).unwrap();
    client.send_datagram(b"query").unwrap();
    let mut bytes = [0; 32];
    let (count, source) = server.recv_from(&mut bytes).unwrap();
    assert_eq!(&bytes[..count], b"query");
    server.send_to(b"reply", source).unwrap();
    let (count, source) = client.recv_from(&mut bytes).unwrap();
    assert_eq!(source, server_address);
    assert_eq!(&bytes[..count], b"reply");
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
        let _environment = sim.enter();
        let r = reactor();
        r.init().unwrap();
        let baseline = r.admission.used(ResourceClass::RequestContext);
        let start = crate::runtime::environment::now();
        let request = crate::runtime::deadline::RequestScope::new(
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
        // No new bytes and no explicit cancellation: the reactor scans local caps.
        assert_eq!(r.poll_budgeted(1), Ok(0));
        assert!(!request.cancellation.is_cancelled());
        assert_eq!(request.deadline.0, start + Duration::from_secs(60));
        if !cancel_first {
            r.state
                .borrow_mut()
                .simulation
                .as_mut()
                .unwrap()
                .completed
                .borrow_mut()
                .swap(0, 1);
        }
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
        if !cancel_first {
            r.state
                .borrow_mut()
                .simulation
                .as_mut()
                .unwrap()
                .completed
                .borrow_mut()
                .swap(0, 1);
        }
        assert_eq!(r.poll_budgeted(1), Ok(1));
        assert!(weak.upgrade().is_some());
        assert_eq!(r.admission.used(ResourceClass::Ciphertext), 19);
        assert_eq!(r.poll_budgeted(1), Ok(1));
        assert!(weak.upgrade().is_none());
        assert_eq!(r.admission.used(ResourceClass::Ciphertext), 0);
    }
}

#[test]
fn abandoned_resources_wait_for_both_fences_in_either_order() {
    for cancel_first in [false, true] {
        let sim = Simulation::new();
        let _environment = sim.enter();
        let r = reactor();
        r.init().unwrap();
        let baseline = r.admission.used(ResourceClass::RequestContext);
        let scope = scope();
        let (fd, peer) = sim.socket_pair();
        let fd = Rc::new(fd);
        let weak = Rc::downgrade(&fd);
        let drops = Rc::new(Cell::new(0));
        let lease = r
            .admission
            .reserve(None, ResourceClass::Connection, 1)
            .unwrap();
        let mut recv = r.recv(
            fd,
            r.file_buffer(8).unwrap(),
            (Probe(drops.clone()), lease),
            &scope,
        );
        assert!(poll(&mut recv).is_pending());
        let id = *r.state.borrow().entries.keys().next().unwrap();
        // An unsolicited cancellation CQE must not mutate the live entry.
        assert!(matches!(
            r.state.borrow_mut().complete(id.0 | CANCEL_BIT, 0),
            Err(Error::Io)
        ));
        drop(recv);
        assert_eq!(r.poll_budgeted(1), Ok(0));
        if !cancel_first {
            // Reorder the two actual driver CQEs, leaving production fence logic intact.
            r.state
                .borrow_mut()
                .simulation
                .as_mut()
                .unwrap()
                .completed
                .borrow_mut()
                .swap(0, 1);
        }
        assert_eq!(r.poll_budgeted(1), Ok(1));
        assert_eq!(drops.get(), 0);
        assert!(weak.upgrade().is_some());
        assert_eq!(r.in_flight(), 1);
        assert_eq!(r.admission.used(ResourceClass::Connection), 1);
        assert!(r.admission.used(ResourceClass::RequestContext) > baseline);
        assert_eq!(r.poll_budgeted(1), Ok(1));
        assert_eq!(drops.get(), 1);
        assert!(weak.upgrade().is_none());
        assert_eq!(r.in_flight(), 0);
        assert_eq!(r.admission.used(ResourceClass::Connection), 0);
        assert_eq!(r.admission.used(ResourceClass::RequestContext), baseline);
        assert!(matches!(
            r.state.borrow_mut().complete(id.0, 8),
            Err(Error::Io)
        ));
        drop(peer);
        assert_eq!(sim.live_handles(), 0);
    }
}

#[test]
fn scheduled_short_io_disconnect_and_budget_preserve_resource_ownership() {
    let sim = Simulation::new();
    let _environment = sim.enter();
    let r = reactor();
    r.init().unwrap();
    let baseline = r.admission.used(ResourceClass::RequestContext);
    let scope = scope();
    let (fd, peer) = sim.socket_pair();
    let fd = Rc::new(fd);
    let drops = Rc::new(Cell::new(0));
    sim.inject("send", Fault::Delay(2));
    sim.set_max_chunk(3);
    let mut first = r.send(
        fd.clone(),
        r.file_bytes(&[1; 8]).unwrap(),
        Probe(drops.clone()),
        &scope,
    );
    assert!(poll(&mut first).is_pending());
    let first_id = *r.state.borrow().entries.keys().next().unwrap();
    sim.inject("send", Fault::Errno(libc::ECONNRESET));
    let mut second = r.send(
        fd.clone(),
        r.file_bytes(&[2; 8]).unwrap(),
        Probe(drops.clone()),
        &scope,
    );
    assert!(poll(&mut second).is_pending());
    assert_eq!(r.poll_budgeted(0), Ok(0));
    assert_eq!(r.poll_budgeted(1), Ok(0));
    assert_eq!(r.poll_budgeted(1), Ok(1));
    assert!(poll(&mut first).is_pending());
    assert!(matches!(
        poll(&mut second),
        std::task::Poll::Ready(Err(Error::Io))
    ));
    assert_eq!(
        drops.get(),
        1,
        "failed I/O releases its owned resources after the CQE"
    );
    drop(second);
    assert_eq!(r.poll_budgeted(1), Ok(0));
    assert_eq!(r.poll_budgeted(0), Ok(0));
    assert!(poll(&mut first).is_pending());
    assert_eq!(r.poll_budgeted(1), Ok(1));
    let std::task::Poll::Ready(Ok(mut completed)) = poll(&mut first) else {
        panic!("short send did not complete")
    };
    drop(first);
    assert_eq!(completed.bytes, 3);
    assert_eq!(completed.buffer.prefix(8).unwrap(), &[1; 8]);
    assert_eq!(drops.get(), 1);
    assert_eq!(completed.buffer.advance(9), Err(Error::Io));
    assert_eq!(completed.buffer.remaining(), 8);
    completed.buffer.advance(3).unwrap();
    let mut remainder = r.send(fd.clone(), completed.buffer, completed.lease, &scope);
    assert!(poll(&mut remainder).is_pending());
    let next_id = *r.state.borrow().entries.keys().next().unwrap();
    assert!(next_id > first_id);
    assert!(matches!(
        r.state.borrow_mut().complete(first_id.0, 3),
        Err(Error::Io)
    ));
    assert_eq!(
        r.in_flight(),
        1,
        "stale CQE cannot retire the new submission"
    );
    sim.disconnect(&fd).unwrap();
    assert!(matches!(drive(&r, remainder), Err(Error::Io)));
    assert_eq!(drops.get(), 2);
    let mut bytes = [0; 8];
    assert_eq!(peer.try_recv(&mut bytes).unwrap(), 3);
    assert_eq!(&bytes[..3], &[1; 3]);
    let eof = drive(
        &r,
        r.recv(Rc::new(peer), r.file_buffer(8).unwrap(), (), &scope),
    )
    .unwrap();
    assert_eq!(eof.bytes, 0);
    drop((eof, fd));
    assert_eq!(r.in_flight(), 0);
    assert_eq!(r.admission.used(ResourceClass::RequestContext), baseline);
    assert_eq!(sim.live_handles(), 0);
    assert!(
        sim.trace()
            .iter()
            .any(|e| e.operation == "submit:send" && e.resource == next_id.0)
    );
}
