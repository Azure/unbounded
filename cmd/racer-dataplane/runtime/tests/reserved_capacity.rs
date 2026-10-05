#![cfg(feature = "simulation")]

use std::{
    rc::Rc,
    task::{Context, Poll},
};
use uring_runtime::{
    Error, Operation, Result, Scope,
    reactor::{Reactor, simulation::Simulation},
};

#[derive(Clone)]
struct OpenScope;

impl Scope for OpenScope {
    type Error = Error;

    fn check(&self) -> Result<()> {
        Ok(())
    }
}

fn poll<T>(operation: &mut Operation<'_, T>) -> Poll<Result<T>> {
    operation
        .as_mut()
        .poll(&mut Context::from_waker(futures::task::noop_waker_ref()))
}

fn complete(reactor: &Reactor<OpenScope, ()>) {
    for _ in 0..16 {
        if reactor.in_flight() == 0 {
            return;
        }
        reactor.poll_budgeted(1).unwrap();
    }
    panic!("simulated operation did not complete in 16 driver turns");
}

#[test]
fn reserved_capacity_is_reactor_local_and_failed_replies_release_it_only_on_consumption() {
    let simulation = Simulation::new();
    let _os = simulation.enter();
    let owner = Reactor::new(1, ());
    let foreign = Reactor::new(1, ());
    let scope = OpenScope;
    let charge = Rc::new(());
    let retained_charge = Rc::downgrade(&charge);
    let capacity = owner.reserve_submissions(1, charge).unwrap();
    let (socket, peer) = simulation.socket_pair();
    let socket = Rc::new(socket);

    // Even an otherwise idle reactor must reject another reactor's capability.
    let mut wrong_owner = foreign.send_reserved(
        socket.clone(),
        foreign.file_bytes(b"wrong").unwrap(),
        capacity.clone(),
        &scope,
    );
    assert!(matches!(
        poll(&mut wrong_owner),
        Poll::Ready(Err(Error::InvalidConfiguration))
    ));
    drop(wrong_owner);
    assert_eq!(foreign.in_flight(), 0);

    // A kernel error still creates a reply that occupies reserved bookkeeping.
    simulation
        .inject(
            "send",
            uring_runtime::reactor::simulation::Fault::Errno(libc::EPIPE),
        )
        .unwrap();
    let mut failed = owner.send_reserved(
        socket.clone(),
        owner.file_bytes(b"failed").unwrap(),
        capacity.clone(),
        &scope,
    );
    assert!(poll(&mut failed).is_pending());
    complete(&owner);
    let mut excess = owner.send_reserved(
        socket.clone(),
        owner.file_bytes(b"excess").unwrap(),
        capacity.clone(),
        &scope,
    );
    assert!(matches!(
        poll(&mut excess),
        Poll::Ready(Err(Error::Overloaded))
    ));
    drop(excess);
    assert!(matches!(
        poll(&mut failed),
        Poll::Ready(Err(Error::Os(libc::EPIPE)))
    ));
    drop(failed);

    // Consuming the error frees the same slot for a successful operation.
    let mut send = owner.send_reserved(
        socket.clone(),
        owner.file_bytes(b"ok").unwrap(),
        capacity.clone(),
        &scope,
    );
    assert!(poll(&mut send).is_pending());
    complete(&owner);
    let mut bytes = [0; 8];
    assert_eq!(peer.try_recv(&mut bytes).unwrap(), 2);
    assert_eq!(&bytes[..2], b"ok", "rejected sends must not publish bytes");

    // Abandoning a completed reply releases the partition's last owner too.
    drop(capacity);
    assert!(retained_charge.upgrade().is_some());
    drop(send);
    assert!(retained_charge.upgrade().is_none());
    let mut ordinary = owner.send(
        socket.clone(),
        owner.file_bytes(b"next").unwrap(),
        (),
        &scope,
    );
    assert!(poll(&mut ordinary).is_pending());
    complete(&owner);
    assert!(matches!(poll(&mut ordinary), Poll::Ready(Ok(completion)) if completion.bytes == 4));
    drop(ordinary);
    assert_eq!(peer.try_recv(&mut bytes).unwrap(), 4);
    assert_eq!(&bytes[..4], b"next");
    assert_eq!(owner.in_flight(), 0);
    drop((socket, peer));
    assert_eq!(simulation.live_handles(), 0);
}

#[test]
fn reserved_receive_and_accept_keep_slots_until_reply_consumption() {
    use uring_runtime::reactor::{SocketAddress, simulation::Fault};
    let simulation = Simulation::new();
    let _os = simulation.enter();
    let reactor = Reactor::new(1, ());
    let capacity = reactor.reserve_submissions(1, ()).unwrap();
    let scope = OpenScope;
    let (socket, peer) = simulation.socket_pair();
    let socket = Rc::new(socket);
    simulation.inject("recv", Fault::HoldCompletion(4)).unwrap();
    let mut recv = reactor.recv_reserved(
        socket.clone(),
        reactor.file_bytes(&[0; 8]).unwrap(),
        capacity.clone(),
        &scope,
    );
    assert!(poll(&mut recv).is_pending());
    peer.try_send(b"ok").unwrap();
    reactor.poll_budgeted(1).unwrap();
    assert_eq!(reactor.in_flight(), 1);
    complete(&reactor);
    let mut blocked = reactor.recv_reserved(
        socket.clone(),
        reactor.file_bytes(&[0]).unwrap(),
        capacity.clone(),
        &scope,
    );
    assert!(matches!(
        poll(&mut blocked),
        Poll::Ready(Err(Error::Overloaded))
    ));
    assert!(matches!(poll(&mut recv), Poll::Ready(Ok(result)) if result.bytes == 2));
    drop((recv, blocked));

    let address = SocketAddress::Inet("127.0.0.1:18080".parse().unwrap());
    let listener = Rc::new(simulation.listen(address.clone()).unwrap());
    let mut accept = reactor.accept_reserved(listener.clone(), Some(capacity.clone()), &scope);
    assert!(poll(&mut accept).is_pending());
    let client = simulation.connect(address).unwrap();
    complete(&reactor);
    let mut blocked = reactor.accept_reserved(listener, Some(capacity), &scope);
    assert!(matches!(
        poll(&mut blocked),
        Poll::Ready(Err(Error::Overloaded))
    ));
    assert!(matches!(poll(&mut accept), Poll::Ready(Ok(_))));
    drop((accept, blocked, client, socket, peer));
    assert_eq!(simulation.live_handles(), 0);
}

#[test]
fn simulated_writes_use_immutable_accessor_like_production() {
    use uring_runtime::reactor::IoBuffer;
    struct ReadOnly(Vec<u8>);
    // SAFETY: private stable Vec; mutation accessor fails without exposing aliases.
    unsafe impl IoBuffer for ReadOnly {
        type Error = Error;
        fn bytes(&self) -> Result<&[u8]> {
            Ok(&self.0)
        }
        fn bytes_mut(&mut self) -> Result<&mut [u8]> {
            Err(Error::InvalidInput)
        }
    }
    let simulation = Simulation::new();
    let _os = simulation.enter();
    let reactor = Reactor::new(1, ());
    let capacity = reactor.reserve_submissions(1, ()).unwrap();
    let scope = OpenScope;
    let (socket, peer) = simulation.socket_pair();
    let mut send =
        reactor.send_reserved(Rc::new(socket), ReadOnly(b"ok".to_vec()), capacity, &scope);
    assert!(poll(&mut send).is_pending());
    complete(&reactor);
    assert!(matches!(poll(&mut send), Poll::Ready(Ok(result)) if result.bytes == 2));
    let mut bytes = [0; 2];
    assert_eq!(peer.try_recv(&mut bytes).unwrap(), 2);
    assert_eq!(&bytes, b"ok");
}

#[test]
fn reserved_failures_and_unread_accepts_do_not_block_other_operations_or_leak_handles() {
    use uring_runtime::reactor::{SocketAddress, simulation::Fault};
    let simulation = Simulation::new();
    let _os = simulation.enter();
    let reactor = Reactor::new(3, ());
    let scope = OpenScope;
    let charge = Rc::new(());
    let weak = Rc::downgrade(&charge);
    let capacity = reactor.reserve_submissions(2, charge).unwrap();
    let address = SocketAddress::Unix("/reserved-errors".into());
    let listener = Rc::new(simulation.listen(address.clone()).unwrap());
    let (socket, peer) = simulation.socket_pair();
    let socket = Rc::new(socket);
    let baseline = simulation.live_handles();
    for accept_error in [false, true] {
        simulation
            .inject("recv", Fault::Errno(libc::ECONNRESET))
            .unwrap();
        if accept_error {
            simulation
                .inject("accept", Fault::Errno(libc::EMFILE))
                .unwrap();
        }
        let mut recv = reactor.recv_reserved(
            socket.clone(),
            reactor.file_buffer(4).unwrap(),
            capacity.clone(),
            &scope,
        );
        let mut accept = reactor.accept_reserved(listener.clone(), Some(capacity.clone()), &scope);
        assert!(poll(&mut recv).is_pending());
        assert!(poll(&mut accept).is_pending());
        let client = (!accept_error).then(|| simulation.connect(address.clone()).unwrap());
        complete(&reactor);
        // Both replies remain unread, but the ordinary partition must progress.
        let mut send = reactor.send(
            socket.clone(),
            reactor.file_bytes(b"ok").unwrap(),
            (),
            &scope,
        );
        assert!(poll(&mut send).is_pending());
        complete(&reactor);
        assert!(matches!(poll(&mut send), Poll::Ready(Ok(done)) if done.bytes == 2));
        let mut bytes = [0; 2];
        assert_eq!(peer.try_recv(&mut bytes).unwrap(), 2);
        assert_eq!(&bytes, b"ok");
        let mut blocked = reactor.recv_reserved(
            socket.clone(),
            reactor.file_buffer(1).unwrap(),
            capacity.clone(),
            &scope,
        );
        assert!(matches!(
            poll(&mut blocked),
            Poll::Ready(Err(Error::Overloaded))
        ));
        assert_eq!(
            simulation.live_handles(),
            baseline + if accept_error { 0 } else { 2 }
        );
        assert!(matches!(
            poll(&mut recv),
            Poll::Ready(Err(Error::Os(libc::ECONNRESET)))
        ));
        if accept_error {
            assert!(matches!(
                poll(&mut accept),
                Poll::Ready(Err(Error::Os(libc::EMFILE)))
            ));
        }
        // Dropping an unread successful accept must reclaim the accepted descriptor.
        drop((recv, accept, send, blocked, client));
        assert_eq!(simulation.live_handles(), baseline);
    }
    drop((capacity, listener, socket, peer));
    assert!(weak.upgrade().is_none());
    assert_eq!(simulation.live_handles(), 0);
}

#[test]
fn reserved_sq_rejection_releases_exact_partition_and_buffer_owner() {
    use std::cell::Cell;
    use uring_runtime::reactor::IoBuffer;
    struct Owned(Vec<u8>, Rc<Cell<usize>>);
    // SAFETY: private non-resizing Vec retains initialized backing across moves.
    unsafe impl IoBuffer for Owned {
        type Error = Error;
        fn bytes(&self) -> Result<&[u8]> {
            Ok(&self.0)
        }
        fn bytes_mut(&mut self) -> Result<&mut [u8]> {
            Ok(&mut self.0)
        }
    }
    impl Drop for Owned {
        fn drop(&mut self) {
            self.1.set(self.1.get() + 1);
        }
    }
    let simulation = Simulation::new();
    let _os = simulation.enter();
    let reactor = Reactor::new(2, ());
    let scope = OpenScope;
    let charge = Rc::new(());
    let charge_weak = Rc::downgrade(&charge);
    let capacity = reactor.reserve_submissions(1, charge).unwrap();
    let (socket, peer) = simulation.socket_pair();
    let socket = Rc::new(socket);
    let drops = Rc::new(Cell::new(0));
    // Occupy the ordinary partition before rejecting a reserved publication.
    let mut ordinary = reactor.recv(socket.clone(), reactor.file_buffer(4).unwrap(), (), &scope);
    assert!(poll(&mut ordinary).is_pending());
    simulation.reject_submissions(1).unwrap();
    let mut rejected = reactor.send_reserved(
        socket.clone(),
        Owned(b"bad".to_vec(), drops.clone()),
        capacity.clone(),
        &scope,
    );
    assert!(matches!(
        poll(&mut rejected),
        Poll::Ready(Err(Error::Overloaded))
    ));
    assert_eq!(drops.get(), 1);
    assert_eq!(reactor.in_flight(), 1);
    let mut excess = reactor.send(
        socket.clone(),
        reactor.file_bytes(b"no").unwrap(),
        (),
        &scope,
    );
    assert!(matches!(
        poll(&mut excess),
        Poll::Ready(Err(Error::Overloaded))
    ));
    let mut reserved = reactor.send_reserved(
        socket.clone(),
        Owned(b"ok".to_vec(), drops.clone()),
        capacity.clone(),
        &scope,
    );
    assert!(
        poll(&mut reserved).is_pending(),
        "same reserved slot must be reusable"
    );
    peer.try_send(b"in").unwrap();
    complete(&reactor);
    assert!(matches!(poll(&mut ordinary), Poll::Ready(Ok(done)) if done.bytes == 2));
    assert!(matches!(poll(&mut reserved), Poll::Ready(Ok(done)) if done.bytes == 2));
    assert_eq!(drops.get(), 2);
    let mut bytes = [0; 8];
    assert_eq!(peer.try_recv(&mut bytes).unwrap(), 2);
    assert_eq!(&bytes[..2], b"ok");
    drop((ordinary, excess, rejected, reserved, capacity, socket, peer));
    assert!(charge_weak.upgrade().is_none());
    assert_eq!(simulation.live_handles(), 0);
}
