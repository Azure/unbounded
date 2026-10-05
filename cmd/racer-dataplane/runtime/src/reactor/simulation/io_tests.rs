//! Assertions migrated from test_support::io onto production Entry ownership.
use super::super::tests::fixtures::{Admission, Limits, Reactor, RequestScope, ResourceClass};
use super::*;
use crate::{Error, Operation, Result};
use std::{
    cell::Cell,
    task::{Context, Poll},
    time::{Duration, Instant},
};

pub(super) fn reactor() -> Reactor {
    Reactor::new(Rc::new(Admission::new(Limits {
        queue_entries: std::num::NonZeroUsize::new(64).unwrap(),
    })))
}
pub(super) fn scope() -> RequestScope {
    RequestScope::new((), Instant::now() + Duration::from_secs(30)).unwrap()
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
    sim.set_stream_capacity(3).unwrap();
    let mut sent = drive(
        &r,
        r.send(client.clone(), r.file_bytes(b"abcdef").unwrap(), (), &scope),
    )
    .unwrap();
    assert_eq!(sent.bytes, 3);
    sent.buffer.advance(sent.bytes).unwrap();
    assert_eq!(sent.buffer.remaining(), 3);
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
    // Reuse the returned owner to finish the request, then send a reply.
    let mut sent = drive(&r, r.send(client.clone(), sent.buffer, (), &scope)).unwrap();
    assert_eq!(sent.bytes, 3);
    sent.buffer.advance(sent.bytes).unwrap();
    assert_eq!(sent.buffer.remaining(), 0);
    drop(sent);
    let read = drive(
        &r,
        r.recv(server.clone(), r.file_buffer(8).unwrap(), (), &scope),
    )
    .unwrap();
    assert_eq!(read.buffer.prefix(read.bytes).unwrap(), b"def");
    drop(read);
    let sent = drive(
        &r,
        r.send(server.clone(), r.file_bytes(b"ok").unwrap(), (), &scope),
    )
    .unwrap();
    assert_eq!(sent.bytes, 2);
    drop(sent);
    let reply = drive(
        &r,
        r.recv(client.clone(), r.file_buffer(8).unwrap(), (), &scope),
    )
    .unwrap();
    assert_eq!(reply.buffer.prefix(reply.bytes).unwrap(), b"ok");
    drop(reply);
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
    assert!(matches!(
        drive(
            &r,
            r.send(server.clone(), r.file_bytes(b"late").unwrap(), (), &scope)
        ),
        Err(Error::Os(libc::EPIPE))
    ));
    drive(&r, r.drain()).unwrap();
    assert_eq!(r.in_flight(), 0);
    assert_eq!(r.init(), Err(Error::Unavailable));
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
    let client = client.into_sim().unwrap();
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
    let (writer, reader) = sim.socket_pair();
    let (writer, reader) = (writer.into_sim().unwrap(), reader.into_sim().unwrap());
    let (pipe_reader, pipe_writer) = sim.pipe(16);
    let (pipe_reader, pipe_writer) = (
        pipe_reader.into_sim().unwrap(),
        pipe_writer.into_sim().unwrap(),
    );
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
    sim.inject("recv", Fault::Short(7)).unwrap();
    assert_eq!(reader.recv(&mut output).unwrap(), 7);
    assert_eq!(&output[..7], &[12, 13, 14, 15, 16, 17, 18]);
    assert_eq!(&output[7..], &[0xcc; 9]);
    assert_eq!(reader.recv(&mut output).unwrap(), 5);
    assert_eq!(&output[..5], &[19, 20, 21, 22, 23]);
    sim.inject("pipe_read", Fault::Short(7)).unwrap();
    assert_eq!(pipe_reader.pipe_read(&mut output).unwrap(), 7);
    assert_eq!(&output[..7], &[12, 13, 14, 15, 16, 17, 18]);
    assert_eq!(pipe_writer.pipe_write(&[24, 25, 26, 27]).unwrap(), 4);
    sim.inject("send", Fault::Errno(libc::EPIPE)).unwrap();
    assert_eq!(
        pipe_reader.splice(&writer, 9).unwrap_err().raw_os_error(),
        Some(libc::EPIPE)
    );
    sim.inject("splice", Fault::Short(6)).unwrap();
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
fn datagrams_preserve_packet_boundaries_and_source_addresses() {
    let sim = Simulation::new();
    let server_address = "127.0.0.1:53".parse().unwrap();
    let server = sim.bind_datagram(server_address).unwrap();
    let client = sim.bind_datagram("127.0.0.1:0".parse().unwrap()).unwrap();
    let (server, client) = (server.into_sim().unwrap(), client.into_sim().unwrap());
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
    sim.inject("send", Fault::Delay(2)).unwrap();
    sim.set_max_chunk(3).unwrap();
    let mut first = r.send(
        fd.clone(),
        r.file_bytes(&[1; 8]).unwrap(),
        Probe(drops.clone()),
        &scope,
    );
    assert!(poll(&mut first).is_pending());
    let first_id = *r.state.borrow().entries.keys().next().unwrap();
    sim.inject("send", Fault::Errno(libc::ECONNRESET)).unwrap();
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
        std::task::Poll::Ready(Err(Error::Os(libc::ECONNRESET)))
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
    assert!(matches!(drive(&r, remainder), Err(Error::Os(libc::EPIPE))));
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
