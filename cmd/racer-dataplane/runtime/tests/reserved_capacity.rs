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
    simulation.inject(
        "send",
        uring_runtime::reactor::simulation::Fault::Errno(libc::EPIPE),
    );
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
    assert!(matches!(poll(&mut failed), Poll::Ready(Err(Error::Io))));
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
