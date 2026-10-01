use super::*;

#[test]
fn constructor_does_not_open_kernel_resources() {
    let reactor = Reactor::new(Rc::new(Admission::new(limits(2))));
    assert!(reactor.state.borrow().ring.is_none());
    assert!(reactor.state.borrow().wake.is_none());
    assert_eq!(reactor.in_flight(), 0);
    assert_eq!(reactor.poll_budgeted(1).unwrap(), 0);
}

#[test]
fn wait_does_not_initialize_absent_ring() {
    let reactor = Reactor::new(Rc::new(Admission::new(limits(2))));
    reactor.wait(Duration::ZERO).unwrap();
    let state = reactor.state.borrow();
    assert!(state.ring.is_none());
    assert!(state.wake.is_none());
    assert!(state.ring_reservation.is_none());
    assert!(state.entries.is_empty());
    assert_eq!(reactor.admission.used(ResourceClass::RequestContext), 0);
}

#[test]
fn submission_classifies_transient_and_fatal_errors() {
    for count in [0, 1, 8] {
        assert_eq!(submission_result(Ok(count)), Ok(()));
    }
    for errno in [libc::EINTR, libc::EAGAIN, libc::EBUSY] {
        assert_eq!(
            submission_result(Err(std::io::Error::from_raw_os_error(errno))),
            Ok(())
        );
    }
    for errno in [libc::EIO, libc::EBADF, libc::EINVAL, libc::ENOMEM] {
        assert_eq!(
            submission_result(Err(std::io::Error::from_raw_os_error(errno))),
            Err(Error::Io)
        );
    }
    assert_eq!(
        submission_result(Err(std::io::Error::other("submission failed"))),
        Err(Error::Io)
    );
}
