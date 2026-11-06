//! Registration mode over the simulated fabric: connection manager setup,
//! per-grant and per-write registrations, synthetic completions, and faults.
use super::{
    simulation::{Device, Fault, Operation, Profile, Simulation, default_profile},
    *,
};

/// A caller-chosen work request id.
const fn wr(n: u64) -> u64 {
    (1 << 56) | n
}

/// Two QPs on one registration-mode device (a loopback connection), a page
/// region, a control region, and the write source region.
struct Pair {
    a: Rc<NativeQueuePair>,

    b: Rc<NativeQueuePair>,

    pages: Rc<NativeRegion>,

    control: Rc<NativeRegion>,

    source: Rc<NativeRegion>,

    quota: Arc<GuardOwner>,
}

/// Discover one registration-mode device in `sim` and build a [`Pair`]
/// with `pages` bytes of page buffers. Nothing is connected yet.
fn pair(sim: &Simulation, pages: usize) -> Pair {
    let _profile = default_profile(Profile::Registration);
    let node = sim
        .with_devices(vec![Device::new(
            "mana0",
            [10, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0, 10, 0, 0, 1],
        )])
        .unwrap();
    let _scope = node.enter();
    let device = Rc::new(discover().unwrap().remove(0));
    assert_eq!(device.mode, Mode::Registration);
    assert_eq!(device.endpoint.gid[10..], [0xff, 0xff, 10, 0, 0, 1]);
    let (quota, _) = lifetime_tests::quota();
    let qp = || {
        NativeQueuePair::new_with_depth(
            device.clone(),
            quota.clone(),
            Arc::new(std::sync::atomic::AtomicBool::new(false)),
            8,
            4,
            &racer_io::Rng::os(),
        )
        .unwrap()
    };
    Pair {
        a: qp(),
        b: qp(),
        pages: NativeRegion::with_access(device.clone(), pages, Access::PAGES, quota.clone())
            .unwrap(),
        control: NativeRegion::with_access(device.clone(), 1024, Access::CONTROL, quota.clone())
            .unwrap(),
        source: NativeRegion::implicit(device, quota.clone()).unwrap(),
        quota,
    }
}

/// Successful completions from one progress call.
fn drain(qp: &NativeQueuePair) -> Result<Vec<WorkCompletion>> {
    let mut out = Vec::new();
    qp.progress_with(|c, _| out.push(c))?;
    Ok(out)
}

/// Poll both sides until both are ready, at most `rounds` times.
fn establish(p: &Pair, rounds: usize) -> bool {
    for _ in 0..rounds {
        if p.a.ready() && p.b.ready() {
            return true;
        }
        drain(&p.a).unwrap();
        drain(&p.b).unwrap();
    }
    p.a.ready() && p.b.ready()
}

/// `b` responds to `a`, `a` initiates, and both become ready.
fn connected(sim: &Simulation, pages: usize) -> Pair {
    let p = pair(sim, pages);
    p.b.connect(p.a.endpoint, false).unwrap();
    p.a.connect(p.b.endpoint, true).unwrap();
    assert!(
        !p.a.ready(),
        "the connection manager connects asynchronously"
    );
    assert!(establish(&p, 8));
    p
}

/// Stop both QPs.
fn stop_both(p: &Pair) {
    p.a.stop().unwrap();
    p.b.stop().unwrap();
}

/// A completion record for comparisons.
fn done(id: u64, opcode: u32, byte_len: u32, imm: Option<u32>) -> WorkCompletion {
    WorkCompletion {
        id,
        opcode,
        byte_len,
        imm,
    }
}

/// `length` bytes of a region at `offset`.
fn peek(region: &NativeRegion, offset: usize, length: usize) -> Vec<u8> {
    unsafe { std::slice::from_raw_parts(region.as_ptr().add(offset), length) }.to_vec()
}

#[test]
/// The connection manager connects the pair; a grant registers the buffer
/// and completes at once; a write registers its source, lands zero copy, and
/// releases its registration on completion; invalidation revokes the key.
fn grant_write_and_invalidate_over_the_connection_manager() {
    let sim = Simulation::new();
    {
        let p = connected(&sim, 8192);
        // No windows and no ODP region on this device.
        assert!(p.pages.raw.is_some() && p.source.is_dynamic());
        p.b.post_recv(wr(1), &p.control, 0, 64).unwrap();
        let window = MemoryWindow::new(&p.b.device).unwrap();
        assert!(window.raw.is_none());
        let key =
            p.b.bind_window(wr(2), &window, &p.pages, 4096, 100, 4096)
                .unwrap();
        assert_eq!(
            drain(&p.b).unwrap(),
            [done(wr(2), opcode::BIND_MW, 0, None)]
        );
        assert_eq!(sim.live_registrations(), 1);
        let bytes: Vec<u8> = (0..100u8).collect();
        let address = bytes.as_ptr() as u64;
        p.a.write_imm(
            wr(3),
            &p.source,
            address,
            100,
            p.pages.address() + 4096,
            key,
            7,
        )
        .unwrap();
        // The source is registered while the write is in flight.
        assert_eq!(sim.live_registrations(), 2);
        assert_eq!(
            drain(&p.a).unwrap(),
            [done(wr(3), opcode::RDMA_WRITE, 0, None)]
        );
        assert_eq!(sim.live_registrations(), 1, "released on completion");
        assert_eq!(
            drain(&p.b).unwrap(),
            [done(wr(1), opcode::RECV_RDMA_WITH_IMM, 100, Some(7))]
        );
        assert_eq!(peek(&p.pages, 4096, 100), bytes);
        p.b.invalidate_window(wr(4), &window, key).unwrap();
        assert_eq!(
            drain(&p.b).unwrap(),
            [done(wr(4), opcode::LOCAL_INV, 0, None)]
        );
        assert_eq!(sim.live_registrations(), 0);
        // A late write with the old key is refused and breaks the link.
        p.b.post_recv(wr(5), &p.control, 0, 64).unwrap();
        p.a.write_imm(
            wr(6),
            &p.source,
            address,
            100,
            p.pages.address() + 4096,
            key,
            8,
        )
        .unwrap();
        assert_eq!(drain(&p.a), Err(Error::Io));
        assert_eq!(p.a.failure().unwrap().status, status::REM_ACCESS_ERR);
        // Invalidating an unbound window is refused before anything runs.
        assert_eq!(
            p.b.invalidate_window(wr(7), &window, key),
            Err(Error::InvalidRequest)
        );
        stop_both(&p);
        assert_eq!(sim.live_registrations(), 0);
        drop(window);
    }
    assert_eq!(sim.live_resources(), 0);
}

#[test]
/// A write longer than one piece goes out in pieces; the receiver reports
/// the whole length and the bare tag.
fn long_writes_go_out_in_pieces_and_report_the_whole_length() {
    let sim = Simulation::new();
    {
        let length = registration::CHUNK as usize * 2 + 4096;
        let p = connected(&sim, length);
        p.b.post_recv(wr(1), &p.control, 0, 64).unwrap();
        let window = MemoryWindow::new(&p.b.device).unwrap();
        let key =
            p.b.bind_window(wr(2), &window, &p.pages, 0, length as u32, length as u64)
                .unwrap();
        drain(&p.b).unwrap();
        let bytes: Vec<u8> = (0..length).map(|i| (i % 251) as u8).collect();
        sim.take_trace();
        p.a.write_imm(
            wr(3),
            &p.source,
            bytes.as_ptr() as u64,
            length as u32,
            p.pages.address(),
            key,
            0x00ab_cdef,
        )
        .unwrap();
        let qpn = p.a.endpoint.qpn;
        let posts: Vec<_> = sim
            .take_trace()
            .into_iter()
            .filter(|e| matches!(e.operation, Operation::Write | Operation::WriteImm))
            .map(|e| (e.operation, e.resource, e.work_id))
            .collect();
        assert_eq!(
            posts,
            [
                (Operation::Write, u64::from(qpn), Some(0)),
                (Operation::Write, u64::from(qpn), Some(0)),
                (Operation::WriteImm, u64::from(qpn), Some(wr(3))),
            ]
        );
        // Only the last piece reports.
        assert_eq!(
            drain(&p.a).unwrap(),
            [done(wr(3), opcode::RDMA_WRITE, 0, None)]
        );
        assert_eq!(
            drain(&p.b).unwrap(),
            [done(
                wr(1),
                opcode::RECV_RDMA_WITH_IMM,
                length as u32,
                Some(0x00ab_cdef)
            )]
        );
        assert!(peek(&p.pages, 0, length) == bytes);
        // Too many pieces, or a tag over 24 bits, is refused before any
        // registration.
        let registrations = sim.live_registrations();
        assert_eq!(
            p.a.write_imm(wr(4), &p.source, 4096, 33 << 20, 0, key, 1),
            Err(Error::Io)
        );
        assert_eq!(
            p.a.write_imm(wr(4), &p.source, 4096, 16, 0, key, 1 << 24),
            Err(Error::InvalidRequest)
        );
        assert_eq!(sim.live_registrations(), registrations);
        stop_both(&p);
        drop(window);
    }
    assert_eq!(sim.live_resources(), 0);
}

#[test]
/// Before the connection is established, sends and writes fail with
/// `Unavailable` and register nothing; receives queue and are replayed when
/// the connection manager creates the QP.
fn posts_before_establishment_wait_or_queue() {
    let sim = Simulation::new();
    {
        let p = pair(&sim, 8192);
        p.b.post_recv(wr(1), &p.control, 0, 64).unwrap();
        p.a.post_recv(wr(2), &p.control, 64, 64).unwrap();
        p.b.connect(p.a.endpoint, false).unwrap();
        p.a.connect(p.b.endpoint, true).unwrap();
        sim.take_trace();
        let bytes = [9u8; 32];
        assert_eq!(
            p.a.write_imm(wr(3), &p.source, bytes.as_ptr() as u64, 32, 0, 0x100, 1),
            Err(Error::Unavailable)
        );
        assert_eq!(p.a.send(wr(3), &p.control, 0, 8), Err(Error::Unavailable));
        assert!(sim.take_trace().is_empty(), "nothing reached the adapter");
        assert!(establish(&p, 8));
        // Both queued receives were replayed: a send lands in b's.
        p.a.send(wr(4), &p.control, 128, 16).unwrap();
        assert_eq!(drain(&p.a).unwrap(), [done(wr(4), opcode::SEND, 0, None)]);
        assert_eq!(drain(&p.b).unwrap(), [done(wr(1), opcode::RECV, 16, None)]);
        p.b.send(wr(5), &p.control, 192, 16).unwrap();
        assert_eq!(drain(&p.b).unwrap(), [done(wr(5), opcode::SEND, 0, None)]);
        assert_eq!(drain(&p.a).unwrap(), [done(wr(2), opcode::RECV, 16, None)]);
        stop_both(&p);
    }
    assert_eq!(sim.live_resources(), 0);
}

#[test]
/// Address, route, and connect failures break the initiator; a connect that
/// never answers leaves it unready (the link's setup timeout breaks it).
fn connection_manager_faults_break_or_stall_the_initiator() {
    for (operation, fault) in [
        (Operation::CmResolveAddr, Fault::AddressError),
        (Operation::CmResolveAddr, Fault::Reject),
        (Operation::CmResolveRoute, Fault::RouteError),
        (Operation::CmResolveRoute, Fault::Reject),
        (Operation::CmCreateQp, Fault::Reject),
        (Operation::CmConnect, Fault::Reject),
        (Operation::CmAccept, Fault::Reject),
        (Operation::CmConnect, Fault::Timeout),
    ] {
        let sim = Simulation::new();
        {
            let p = pair(&sim, 8192);
            p.b.connect(p.a.endpoint, false).unwrap();
            sim.fault(operation, fault);
            if fault == Fault::Reject && operation == Operation::CmResolveAddr {
                // Resolution starts in connect.
                assert_eq!(p.a.connect(p.b.endpoint, true), Err(Error::Unavailable));
                assert!(p.a.stopped());
            } else {
                p.a.connect(p.b.endpoint, true).unwrap();
                let mut broke = false;
                for _ in 0..8 {
                    broke |= drain(&p.a).is_err();
                    let _ = drain(&p.b);
                }
                let expected = fault != Fault::Timeout;
                assert_eq!(broke, expected, "{operation:?} {fault:?}");
                assert!(!p.a.ready());
                if operation == Operation::CmAccept {
                    // The responder's accepted QP is torn down too.
                    assert!(drain(&p.b).is_err() || p.b.stopped());
                }
            }
            assert_eq!(sim.pending_faults(), 0);
            stop_both(&p);
        }
        assert_eq!(sim.live_resources(), 0, "{operation:?} {fault:?}");
    }
}

#[test]
/// A request for a token nobody registered, or from an unexpected
/// initiator, is rejected and its id destroyed; the initiator breaks.
fn unexpected_requests_are_rejected_and_destroyed() {
    for expected_peer in [None, Some(0x00ab_cdef)] {
        let sim = Simulation::new();
        {
            let p = pair(&sim, 8192);
            if let Some(token) = expected_peer {
                let wrong = Endpoint {
                    qpn: token,
                    ..p.a.endpoint
                };
                p.b.connect(wrong, false).unwrap();
            }
            p.a.connect(p.b.endpoint, true).unwrap();
            let mut broke = false;
            for _ in 0..8 {
                broke |= drain(&p.a).is_err();
                drain(&p.b).unwrap();
            }
            assert!(broke);
            assert!(!p.b.ready());
            let destroyed = sim
                .trace()
                .iter()
                .filter(|e| e.operation == Operation::CmDestroyId && e.result == 0)
                .count();
            assert_eq!(destroyed, 2, "the rejected id and the initiator's");
            stop_both(&p);
        }
        assert_eq!(sim.live_resources(), 0);
    }
}

#[test]
/// When one side stops, the other sees the disconnect and breaks.
fn disconnect_breaks_the_peer() {
    let sim = Simulation::new();
    {
        let p = connected(&sim, 8192);
        p.b.post_recv(wr(1), &p.control, 0, 64).unwrap();
        p.a.stop().unwrap();
        // The responder flushes its receive and breaks.
        p.b.device.progress();
        assert_eq!(drain(&p.b), Err(Error::Io));
        assert!(p.b.stopped());
    }
    assert_eq!(sim.live_resources(), 0);
}

#[test]
/// A failed source or grant registration fails the post and registers
/// nothing; a failed deregistration keeps the registration until stop, and
/// one that still fails then leaks, counted against the charge.
fn registration_and_deregistration_failures() {
    let sim = Simulation::new();
    {
        let p = connected(&sim, 8192);
        let window = MemoryWindow::new(&p.b.device).unwrap();
        sim.fault(Operation::RegMr, Fault::Reject);
        assert_eq!(
            p.b.bind_window(wr(1), &window, &p.pages, 0, 64, 4096),
            Err(Error::Io)
        );
        assert_eq!(sim.live_registrations(), 0);
        let key =
            p.b.bind_window(wr(1), &window, &p.pages, 0, 64, 4096)
                .unwrap();
        drain(&p.b).unwrap();
        let bytes = [3u8; 64];
        let address = bytes.as_ptr() as u64;
        sim.fault(Operation::RegMr, Fault::Reject);
        assert_eq!(
            p.a.write_imm(wr(2), &p.source, address, 64, p.pages.address(), key, 1),
            Err(Error::Io)
        );
        assert_eq!(sim.live_registrations(), 1, "only the grant");
        // A failed invalidation keeps the grant registered.
        sim.fault(Operation::DeregMr, Fault::Reject);
        assert_eq!(p.b.invalidate_window(wr(3), &window, key), Err(Error::Io));
        assert_eq!(sim.live_registrations(), 1);
        // A source whose deregistration keeps failing outlives the write.
        p.b.post_recv(wr(4), &p.control, 0, 64).unwrap();
        p.a.write_imm(wr(5), &p.source, address, 64, p.pages.address(), key, 1)
            .unwrap();
        sim.reject(Operation::DeregMr, None, true);
        assert_eq!(drain(&p.a).unwrap().len(), 1);
        assert_eq!(sim.live_registrations(), 2);
        assert!(!p.quota.quarantined(), "kept for a retry at stop");
        p.a.stop().unwrap();
        assert_eq!(p.a.device.leaked_registrations(), 1);
        assert!(p.quota.quarantined(), "the leak is counted");
        sim.reject(Operation::DeregMr, None, false);
        p.b.stop().unwrap();
        drop(window);
        assert_eq!(sim.live_registrations(), 1, "only the leaked source");
    }
}

#[test]
/// Grants round up to whole 2 MiB pages within the granted buffer only.
fn grant_rounding_is_bounded_by_the_buffer() {
    let sim = Simulation::new();
    {
        let huge = registration::HUGE as usize;
        let p = connected(&sim, 2 * huge);
        // The simulated region is not 2 MiB aligned, so check the span rule
        // directly and the bound through a write past the grant.
        let window = MemoryWindow::new(&p.b.device).unwrap();
        let key =
            p.b.bind_window(wr(1), &window, &p.pages, 0, 100, 4096)
                .unwrap();
        drain(&p.b).unwrap();
        p.b.post_recv(wr(2), &p.control, 0, 64).unwrap();
        let bytes = [1u8; 200];
        p.a.write_imm(
            wr(3),
            &p.source,
            bytes.as_ptr() as u64,
            200,
            p.pages.address(),
            key,
            1,
        )
        .unwrap();
        assert_eq!(drain(&p.a), Err(Error::Io), "past the grant");
        assert_eq!(registration::grant_span(0, 100, 4096), 100);
        assert_eq!(registration::grant_span(0, 100, huge as u64), huge as u64);
        stop_both(&p);
        drop(window);
    }
    assert_eq!(sim.live_resources(), 0);
}

#[test]
/// Activation-time checks: a device whose messages are too small for a
/// piece is skipped at discovery.
fn small_message_devices_are_skipped() {
    let sim = Simulation::new();
    sim.max_message(1 << 20);
    let _profile = default_profile(Profile::Registration);
    let node = sim
        .with_devices(vec![Device::new("mana0", [7; 16])])
        .unwrap();
    let _scope = node.enter();
    assert!(discover().unwrap().is_empty());
    assert_eq!(sim.live_resources(), 0);
}

#[test]
/// The completion channel and connection manager fds are real and become
/// readable when there is work: event-driven progress needs no busy poll.
fn channel_fds_signal_work() {
    /// True if `fd` is readable now.
    fn readable(fd: c_int) -> bool {
        let mut poll = libc::pollfd {
            fd,
            events: libc::POLLIN,
            revents: 0,
        };
        unsafe { libc::poll(&mut poll, 1, 0) == 1 }
    }

    let sim = Simulation::new();
    {
        let p = pair(&sim, 8192);
        let fds = p.a.device.fds();
        assert_eq!(fds.len(), 2);
        let (comp, cm) = (fds[0], fds[1]);
        // fds_into appends to a reused buffer rather than replacing it.
        let mut reused = vec![-7];
        p.a.device.fds_into(&mut reused);
        assert_eq!(reused, [-7, comp, cm]);
        assert!(!readable(comp) && !readable(cm));
        p.b.connect(p.a.endpoint, false).unwrap();
        p.a.connect(p.b.endpoint, true).unwrap();
        assert!(readable(cm), "address resolved");
        assert!(establish(&p, 8));
        assert!(!readable(cm), "every event consumed");
        // Both CQs are armed by their last poll; a send raises an event.
        p.b.post_recv(wr(1), &p.control, 0, 64).unwrap();
        p.a.send(wr(2), &p.control, 64, 8).unwrap();
        assert!(readable(comp));
        p.a.device.progress();
        assert!(!readable(comp));
        // Re-armed and re-polled: the send completes, and the receive
        // raises the next event.
        assert_eq!(drain(&p.a).unwrap(), [done(wr(2), opcode::SEND, 0, None)]);
        assert!(readable(comp));
        p.a.device.progress();
        assert_eq!(drain(&p.b).unwrap(), [done(wr(1), opcode::RECV, 8, None)]);
        stop_both(&p);
    }
    assert_eq!(sim.live_resources(), 0);
}
