// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

impl super::TlsChannel {
    pub(crate) fn assert_offload_for_test(&self) {
        crate::tls::tests::assert_offload(&self.session);
    }
}

#[test]
fn fatal_record_error_rejects_cached_admission_and_pending_io() {
    use super::*;
    use crate::tls::{ExpectedPeer, PeerIdentity, tests::Authority};
    use std::{net::TcpListener, net::TcpStream, time::Duration};

    let Some(mut ring) = crate::conformance::kernel_ring(4, Default::default()) else {
        return;
    };
    let ca = Authority::new();
    let identity =
        |node: &str| PeerIdentity::new(&"a".repeat(64), &node.repeat(64), "pod").unwrap();
    let a = identity("b");
    let b = identity("c");
    let listener = TcpListener::bind("127.0.0.1:0").unwrap();
    let socket = TcpStream::connect(listener.local_addr().unwrap()).unwrap();
    let accepted = listener.accept().unwrap().0;
    let mut client = TlsChannel::new(
        File::new(socket.into()),
        &ca.context(&a),
        ExpectedPeer::Identity(b.clone()),
        false,
    )
    .unwrap();
    let mut server = TlsChannel::new(
        File::new(accepted.into()),
        &ca.context(&b),
        ExpectedPeer::Identity(a),
        true,
    )
    .unwrap();
    let end = crate::environment::now() + Duration::from_secs(5);
    while !client.admits_new_request() || !server.admits_new_request() {
        assert!(crate::environment::now() < end);
        ring.progress().unwrap();
        client.handshake(&mut ring, end).unwrap();
        server.handshake(&mut ring, end).unwrap();
    }
    client.assert_offload_for_test();
    server.assert_offload_for_test();
    assert!(matches!(
        client.poll_read(&mut ring, &mut [0; 8], end).unwrap(),
        Progress::Pending(_)
    ));
    crate::tls::tests::send_fatal_alert(&server.session);
    loop {
        assert!(crate::environment::now() < end);
        ring.progress().unwrap();
        match client.poll_read(&mut ring, &mut [0; 8], end) {
            Err(_) => break,
            Ok(Progress::Pending(_)) => (),
            Ok(Progress::Ready(n)) => panic!("fatal alert returned {n} application bytes"),
        }
    }
    assert!(client.session.failed);
    // A caller ignoring the fatal error must not regain admission or wait for
    // stale readiness before observing the terminal state on another operation.
    client.write_ready = Some(
        ring.poll_fd(
            client.file.clone().into(),
            crate::uring::Readiness::Readable,
        )
        .unwrap()
        .cancel_on_drop(),
    );
    let file = File::new(std::fs::File::open("Cargo.toml").unwrap().into());
    for _ in 0..2 {
        assert!(!client.admits_new_request());
        assert!(client.handshake(&mut ring, end).is_err());
        assert!(client.poll_read(&mut ring, &mut [], end).is_err());
        assert!(client.poll_write(&mut ring, b"", end).is_err());
        assert!(client.poll_write(&mut ring, b"rejected", end).is_err());
        assert!(client.poll_sendfile(&mut ring, &file, 0, 0, end).is_err());
        assert!(client.poll_sendfile(&mut ring, &file, 0, 1, end).is_err());
    }
    drop(client);
    drop(server);
    ring.shutdown().unwrap();
}
