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
    assert_eq!(capacity.active.get(), 1);
    let mut excess = reactor.send_reserved(fd.clone(), buffer(b"y"), capacity.clone(), &request);
    assert!(matches!(
        poll(&mut excess),
        Poll::Ready(Err(Error::Overloaded))
    ));
    assert!(matches!(poll(&mut send), Poll::Ready(Ok(_))));
    drop(send);
    assert_eq!(capacity.active.get(), 0);
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

#[test]
fn ordinary_completed_reply_does_not_hold_queue_capacity() {
    let reactor = kernel_reactor(1).expect("real io_uring ordinary completion");
    let request = scope();
    let (socket, _peer) = UnixStream::pair().unwrap();
    let fd = Rc::new(Descriptor::from(socket));
    let mut send = reactor.send(fd.clone(), buffer(b"x"), (), &request);
    assert!(poll(&mut send).is_pending());
    let deadline = Instant::now() + Duration::from_secs(2);
    while reactor.in_flight() != 0 {
        assert!(Instant::now() < deadline);
        reactor.poll_budgeted(8).unwrap();
        reactor.wait(Duration::from_millis(1)).unwrap();
    }
    // Ordinary reply memory remains dynamically charged, not prepaid. Preserve
    // the original queue semantics while the completed result is still unpolled.
    assert_eq!(reactor.ordinary.get(), 0);
    let mut receive = reactor.recv(fd, buffer(&[0]), (), &request);
    assert!(poll(&mut receive).is_pending());
    assert!(matches!(poll(&mut send), Poll::Ready(Ok(_))));
    drop((send, receive));
    drive(&reactor, reactor.drain()).unwrap();
    assert_eq!(reactor.ordinary.get(), 0);
}
