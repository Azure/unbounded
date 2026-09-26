//! Assertions migrated from test_support::io onto production Entry ownership.
use super::{tests::*, *};
use crate::{error::Error, model::limits::ResourceClass};
use std::cell::Cell;

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
        let mut recv = r.recv(fd, r.file_buffer(8).unwrap(), Probe(drops.clone()), &scope);
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
        assert!(r.admission.used(ResourceClass::RequestContext) > baseline);
        assert_eq!(r.poll_budgeted(1), Ok(1));
        assert_eq!(drops.get(), 1);
        assert!(weak.upgrade().is_none());
        assert_eq!(r.in_flight(), 0);
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
