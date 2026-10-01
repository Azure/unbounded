//! Exercise release credit through the real duplex response, not release_page.
use super::*;
use crate::{client::response::Responses, http::connection::ConnectionLease};
use std::io::{Read, Write};

#[test]
fn duplex_release_replenishes_acquisition_while_next_page_write_is_blocked() {
    run("valid");
}

#[test]
fn duplex_release_malformed_length_and_incomplete_page_are_rejected_while_blocked() {
    run("malformed");
    run("incomplete");
}

#[test]
fn duplex_release_eof_cancellation_and_drop_drain_owned_write_fences() {
    run("eof");
    run("cancel");
    run("drop");
}

#[test]
fn duplex_release_stalled_writer_expires_but_progressing_writer_outlives_read_deadline() {
    run("deadline");
    run("progress");
}

#[test]
fn duplex_release_partial_frame_survives_delivery_to_next_slice_transition() {
    run("transition");
}

fn run(mode: &str) {
    let clock = crate::runtime::environment::SimulationClock::new(311);
    let environment = clock.environment(0);
    let _clock = environment.enter();
    let signers = network(1);
    let mut f = fixture_with(2 * PAGE_BYTES + 7, None);
    f.reactor.init().unwrap();
    let membership = Arc::new(
        Membership::validate(
            MembershipVersion(1),
            vec![Member {
                node: signers[0].node().clone(),
                shares: NonZeroU32::new(1).unwrap(),
                peer_endpoint: "127.0.0.1:8000".into(),
                rails: vec![],
                alignment_enabled: false,
            }],
        )
        .unwrap(),
    );
    let (local, mut endpoint, _publication) =
        coordinator(&f, &signers[0], &membership, Rc::new(NoPeer));
    let scope = RequestScope::new(
        f.scope.request,
        crate::runtime::environment::now() + Duration::from_secs(60),
    )
    .unwrap();
    let request = ClientRequest {
        kind: ReadKind::Subscription {
            pin: Some(f.page.version.etag.clone()),
            range: Some(ByteRange::From(PAGE_BYTES - 1)),
            page_credits: 2,
            byte_credits: 2 * PAGE_BYTES,
            ordered: true,
        },
        origin: OriginContext {
            object: f.context.object.clone(),
            metadata: None,
            authorization: None,
        },
    };
    let response = futures::executor::block_on(local.read(request, &scope)).unwrap();
    let admission = f.fill.dependencies.admission.clone();
    let delivery = Rc::new(Delivery::new(
        Rc::new(PipePool::new(admission.clone(), f.reactor.clone())),
        Duration::from_secs(30),
    ));
    let io = Rc::new(HttpIo::with_admission(
        f.reactor.clone(),
        Codec::new(32768, i64::MAX as u64),
        admission.clone(),
    ));
    let responses = Responses::new(io, delivery.clone());
    let (socket, mut client) = std::os::unix::net::UnixStream::pair().unwrap();
    client.set_nonblocking(true).unwrap();
    let connection = ConnectionLease::from_accepted(socket.into(), &admission).unwrap();
    let mut send = responses.send_subscription_unobserved(
        connection,
        response,
        &scope,
        Duration::from_secs(30),
    );
    #[derive(Default)]
    struct Wakes(std::sync::atomic::AtomicUsize);
    impl futures::task::ArcWake for Wakes {
        fn wake_by_ref(this: &Arc<Self>) {
            this.0.fetch_add(1, std::sync::atomic::Ordering::Relaxed);
        }
    }
    let wakes = Arc::new(Wakes::default());
    let waker = futures::task::waker(wakes.clone());
    let mut cx = Context::from_waker(&waker);
    let mut pump = |endpoint: &mut crate::read::dispatch::WorkerEndpoint| {
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        endpoint.poll(&mut cx, 64).unwrap();
        crate::read::drivers::poll(&mut cx, 64);
        f.engine.poll_budgeted(64).unwrap();
        f.crypto.poll_budgeted(64).unwrap();
        f.reactor.poll_budgeted(128).unwrap();
    };
    // Read just the HTTP head, complete one-byte page zero, and page-one frame.
    // Leaving the entire page-one payload unread forces actual socket backpressure.
    let mut prefix = Vec::new();
    let mut head_end = None;
    for _ in 0..8192 {
        assert!(send.as_mut().poll(&mut cx).is_pending());
        pump(&mut endpoint);
        let mut byte = [0];
        match client.read(&mut byte) {
            Ok(1) => prefix.push(byte[0]),
            Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => {}
            other => panic!("prefix read: {other:?}"),
        }
        if head_end.is_none() && prefix.ends_with(b"\r\n\r\n") {
            head_end = Some(prefix.len());
        }
        if head_end.is_some_and(|end| prefix.len() == end + 43) {
            break;
        }
    }
    let end = head_end.expect("HTTP response head");
    assert_eq!(prefix.len(), end + 43);
    let page_frame = |number: u64, offset: u64, length: u32| {
        let mut frame = vec![1];
        frame.extend(number.to_be_bytes());
        frame.extend(offset.to_be_bytes());
        frame.extend(length.to_be_bytes());
        frame
    };
    assert_eq!(&prefix[end..end + 21], page_frame(0, PAGE_BYTES - 1, 1));
    assert_eq!(prefix[end + 21], 0);
    assert_eq!(
        &prefix[end + 22..],
        page_frame(1, PAGE_BYTES, PAGE_BYTES as u32)
    );
    for _ in 0..128 {
        assert!(send.as_mut().poll(&mut cx).is_pending());
        pump(&mut endpoint);
    }
    assert_eq!(&*f.origin.started_pages.borrow(), &[0, 1]);
    assert!(
        f.reactor.in_flight() > 0,
        "blocked send owns a completion fence"
    );

    // An independent reader retains the same immutable page with its own pipe.
    let other_scope = RequestScope::new(f.scope.request, scope.deadline.0).unwrap();
    let other = delivery
        .attach(
            f.fill
                .dependencies
                .memory
                .get(&f.page)
                .unwrap()
                .unwrap()
                .plaintext,
            crate::model::PageSlice {
                page: PageNumber(0),
                offset: (PAGE_BYTES - 1) as u32,
                length: 1,
            },
        )
        .unwrap();
    let mut release = [0; 12];
    release[8..].copy_from_slice(&1u32.to_be_bytes());
    // Pump only the reactor after sending input. A blocked writer must be woken
    // by POLLIN, rather than relying on unrelated output/acquisition wakeups.
    let before = wakes.0.load(std::sync::atomic::Ordering::Relaxed);
    client.write_all(&release[..5]).unwrap();
    for _ in 0..128 {
        pump(&mut endpoint);
        if wakes.0.load(std::sync::atomic::Ordering::Relaxed) > before {
            break;
        }
    }
    assert!(
        wakes.0.load(std::sync::atomic::Ordering::Relaxed) > before,
        "release input wakes blocked delivery"
    );
    for _ in 0..128 {
        assert!(send.as_mut().poll(&mut cx).is_pending());
        pump(&mut endpoint);
    }
    assert_eq!(
        &*f.origin.started_pages.borrow(),
        &[0, 1],
        "partial release grants no credit"
    );
    let mut received = Vec::new();
    if matches!(
        mode,
        "malformed" | "incomplete" | "eof" | "cancel" | "drop" | "deadline"
    ) {
        let expected = match mode {
            "malformed" => {
                release[8..].copy_from_slice(&2u32.to_be_bytes());
                client.write_all(&release[5..]).unwrap();
                Error::InvalidRequest
            }
            "incomplete" => {
                release[5..8].copy_from_slice(&[0, 0, 1]);
                release[8..].copy_from_slice(&(PAGE_BYTES as u32).to_be_bytes());
                client.write_all(&release[5..]).unwrap();
                Error::InvalidRequest
            }
            "eof" => {
                client.shutdown(std::net::Shutdown::Write).unwrap();
                Error::Io
            }
            "cancel" => {
                scope.cancel().unwrap();
                Error::Cancelled
            }
            "deadline" => {
                clock.advance(Duration::from_secs(31));
                Error::DeadlineExceeded
            }
            "drop" => Error::Cancelled,
            _ => unreachable!(),
        };
        if mode != "drop" {
            let mut error = None;
            for _ in 0..1024 {
                if let Poll::Ready(result) = send.as_mut().poll(&mut cx) {
                    error = Some(result.err().expect("blocked response must fail"));
                    break;
                }
                pump(&mut endpoint);
            }
            assert_eq!(error, Some(expected), "{mode}");
        }
        drop(send);
        if matches!(mode, "malformed" | "incomplete" | "eof" | "drop") {
            assert!(
                f.reactor.in_flight() > 0,
                "abandonment does not drop accepted write ownership before its fence"
            );
        }
        assert_eq!(
            &*f.origin.started_pages.borrow(),
            &[0, 1],
            "invalid release never grants acquisition credit"
        );
    } else {
        if mode == "transition" {
            // Finish page one with the page-zero release still incomplete. The
            // next_slice credit wait must resume that parser, not start a new frame.
            for _ in 0..8192 {
                assert!(send.as_mut().poll(&mut cx).is_pending());
                pump(&mut endpoint);
                let mut bytes = [0; 65536];
                match client.read(&mut bytes) {
                    Ok(n) => received.extend_from_slice(&bytes[..n]),
                    Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => {}
                    other => panic!("transition read: {other:?}"),
                }
                if received.len() == PAGE_BYTES as usize {
                    break;
                }
            }
            assert_eq!(received.len(), PAGE_BYTES as usize);
            for _ in 0..128 {
                assert!(send.as_mut().poll(&mut cx).is_pending());
                pump(&mut endpoint);
            }
            assert_eq!(&*f.origin.started_pages.borrow(), &[0, 1]);
        }
        if mode == "progress" {
            // Free socket capacity and let delivery make progress before the read
            // alarm expires. The receive side must not impose a total page deadline.
            clock.advance(Duration::from_secs(20));
            for _ in 0..32 {
                let mut bytes = [0; 65536];
                match client.read(&mut bytes) {
                    Ok(n) if n > 0 => received.extend_from_slice(&bytes[..n]),
                    Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => break,
                    other => panic!("progress drain: {other:?}"),
                }
            }
            assert!(!received.is_empty());
            for _ in 0..128 {
                assert!(send.as_mut().poll(&mut cx).is_pending());
                pump(&mut endpoint);
            }
            let mut bytes = [0; 65536];
            let n = client
                .read(&mut bytes)
                .expect("writer refilled the drained socket");
            assert!(n > 0);
            received.extend_from_slice(&bytes[..n]);
            clock.advance(Duration::from_secs(11));
            for _ in 0..128 {
                assert!(send.as_mut().poll(&mut cx).is_pending());
                pump(&mut endpoint);
            }
        }
        client.write_all(&release[5..]).unwrap();
        let third = PageId {
            version: f.page.version.clone(),
            number: PageNumber(2),
        };
        for _ in 0..4096 {
            assert!(send.as_mut().poll(&mut cx).is_pending());
            pump(&mut endpoint);
            if f.fill.dependencies.memory.get(&third).unwrap().is_some() {
                break;
            }
        }
        assert_eq!(
            &*f.origin.started_pages.borrow(),
            &[0, 1, 2],
            "wire release must refill acquisition before page-one write completes"
        );
        assert!(f.fill.dependencies.memory.get(&third).unwrap().is_some());

        let mut done = None;
        for _ in 0..8192 {
            if done.is_none()
                && let Poll::Ready(result) = send.as_mut().poll(&mut cx)
            {
                done = Some(result.unwrap());
            }
            pump(&mut endpoint);
            let mut bytes = [0; 65536];
            match client.read(&mut bytes) {
                Ok(n) => received.extend_from_slice(&bytes[..n]),
                Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => {}
                other => panic!("payload read: {other:?}"),
            }
            if done.is_some() && received.len() == PAGE_BYTES as usize + 21 + 7 + 21 {
                break;
            }
        }
        assert!(
            done.is_some(),
            "subscription completed without final releases"
        );
        let n = PAGE_BYTES as usize;
        assert_eq!(received.len(), n + 49);
        assert!(received[..n].iter().all(|b| *b == 1));
        assert_eq!(&received[n..n + 21], page_frame(2, 2 * PAGE_BYTES, 7));
        assert_eq!(&received[n + 21..n + 28], &[2; 7]);
        let mut completion = vec![2];
        completion.extend(3u64.to_be_bytes());
        completion.extend((PAGE_BYTES + 8).to_be_bytes());
        completion.extend(0u32.to_be_bytes());
        assert_eq!(&received[n + 28..], completion);
        drop((send, done));
    }
    assert_eq!(other.bytes_sent(), 0);
    assert!(!other_scope.cancellation.is_cancelled());
    let (socket, mut independent_client) = std::os::unix::net::UnixStream::pair().unwrap();
    independent_client
        .set_read_timeout(Some(Duration::from_secs(1)))
        .unwrap();
    let mut other = other;
    other.attach_connection(socket.into()).unwrap();
    futures::executor::block_on(delivery.finish(other, &other_scope)).unwrap();
    let mut byte = [255];
    independent_client.read_exact(&mut byte).unwrap();
    assert_eq!(byte, [0], "other reader retains its own page and cursor");
    for _ in 0..128 {
        pump(&mut endpoint);
    }
    assert_eq!(
        f.reactor.in_flight(),
        0,
        "all socket completion fences drained"
    );
    endpoint.uninstall().unwrap();
}
