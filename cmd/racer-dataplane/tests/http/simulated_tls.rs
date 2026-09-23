// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

use super::*;
use crate::{http_client as client, simulation::World, tls::PeerIdentity};

fn identity(node: &str) -> PeerIdentity {
    PeerIdentity::new(&"01".repeat(32), &node.repeat(32), "pod").unwrap()
}

fn turn(world: &World, ring: &mut Ring) {
    world.advance(Duration::from_millis(1));
    ring.progress().unwrap();
}

#[test]
fn simulated_tls_expiry_rejects_admission_but_preserves_accepted_response() {
    for client_expires_first in [false, true] {
        let run = |replay| {
            let world = World::new(812);
            let _scope = world.enter();
            world.enable_scheduler();
            if let Some(choices) = replay {
                world.replay(choices);
            }
            let mut ring =
                Ring::http_test_ring(crate::buffers::io_test_pool(4), Default::default()).unwrap();
            let address = "127.0.0.1:9443".parse().unwrap();
            let mut listener = Listener::bind(address, NonZeroU32::new(8).unwrap()).unwrap();
            let now = crate::environment::wall()
                .duration_since(std::time::UNIX_EPOCH)
                .unwrap()
                .as_secs();
            let expiry = now + 60;
            listener.tls_revision = 7;
            listener.tls_expiry = if client_expires_first {
                expiry + 100
            } else {
                expiry
            };
            listener.set_simulated_tls(identity("03"));
            let mut connection = client::Connection::new_simulated_peer(
                address,
                "localhost",
                identity("02"),
                identity("03"),
            )
            .unwrap();
            connection.set_tls_revision(
                5,
                if client_expires_first {
                    expiry
                } else {
                    expiry + 100
                },
            );
            let deadline = world.now() + Duration::from_secs(20);
            let mut exchange = connection
                .head(client::Request::new("/accepted", &[]).unwrap(), deadline)
                .unwrap();
            let accepted = loop {
                turn(&world, &mut ring);
                assert!(matches!(
                    exchange.poll(&mut ring, 32).unwrap(),
                    Progress::Pending(_)
                ));
                if let Progress::Ready(connection) = listener.poll_accept(&mut ring, 32).unwrap() {
                    break connection;
                }
                assert!(world.now() < deadline);
            };
            assert_eq!(accepted.simulated_tls().revision, 7);
            assert_eq!(accepted.simulated_tls().admission.expires_unix, expiry);
            let mut receiving = accepted.receive(deadline);
            let request = loop {
                turn(&world, &mut ring);
                assert!(matches!(
                    exchange.poll(&mut ring, 32).unwrap(),
                    Progress::Pending(_)
                ));
                if let Progress::Ready(request) = receiving.poll(&mut ring, 32).unwrap() {
                    break request;
                }
                assert!(world.now() < deadline);
            };
            let monotonic = world.now();
            world.wall_offset(None, 60_000);
            assert_eq!(world.now(), monotonic);
            // A listener rotation cannot renew the already authenticated socket.
            listener.tls_revision = 8;
            listener.tls_expiry = expiry + 1000;
            listener.set_simulated_tls(identity("03"));
            let Request::Head(request) = request else {
                panic!("expected HEAD")
            };
            let mut sending = request
                .respond(ResponseHead::new(200, Some(0), &[]).unwrap())
                .unwrap();
            let done = loop {
                turn(&world, &mut ring);
                if let Progress::Ready(done) = sending.poll(&mut ring, 32).unwrap() {
                    break done;
                }
                assert!(world.now() < deadline);
            };
            let response = loop {
                turn(&world, &mut ring);
                if let Progress::Ready(response) = exchange.poll(&mut ring, 32).unwrap() {
                    break response;
                }
                assert!(world.now() < deadline);
            };
            assert_eq!(response.status(), 200);
            let connection = response.recycle().unwrap();
            assert_eq!(connection.simulated_tls().revision, 5);
            assert!(connection.simulated_tls().admission.expired(u64::MAX));
            let mut done = done;
            let accepted = done.take_connection().unwrap();
            assert_eq!(accepted.simulated_tls().revision, 7);
            let mut next = accepted.receive(deadline);
            assert_eq!(
                next.poll(&mut ring, 32).err().unwrap().kind(),
                io::ErrorKind::ConnectionAborted
            );
            let mut origin =
                client::Origin::new(client::Endpoint::parse(&address.to_string()).unwrap());
            origin.recycle(Some(connection));
            origin.maintain();
            // No expired socket can be returned from the idle pool.
            let (fresh, permit) = origin.connection().unwrap();
            assert!(fresh.peer_identity().is_none());
            drop((
                fresh, permit, origin, next, done, receiving, sending, exchange, listener,
            ));
            ring.shutdown().unwrap();
            drop(ring);
            world.assert_clean();
            world.assert_replay_consumed();
            (world.digest(), world.choices())
        };
        let (digest, choices) = run(None);
        assert_eq!(run(Some(choices.clone())), (digest, choices));
    }
}

#[test]
fn simulated_tls_expired_handshake_and_monotonic_age_are_bounded() {
    let world = World::new(813);
    let _scope = world.enter();
    let mut ring =
        Ring::http_test_ring(crate::buffers::io_test_pool(4), Default::default()).unwrap();
    let address = "127.0.0.1:9443".parse().unwrap();
    let mut listener = Listener::bind(address, NonZeroU32::new(8).unwrap()).unwrap();
    listener.tls_expiry = 0;
    listener.set_simulated_tls(identity("03"));
    let connection = client::Connection::new_simulated_peer(
        address,
        "localhost",
        identity("02"),
        identity("03"),
    )
    .unwrap();
    let end = world.now() + Duration::from_secs(5);
    let mut exchange = connection
        .head(client::Request::new("/expired", &[]).unwrap(), end)
        .unwrap();
    loop {
        turn(&world, &mut ring);
        match exchange.poll(&mut ring, 32) {
            Err(error) => {
                assert_eq!(error.kind(), io::ErrorKind::ConnectionAborted);
                break;
            }
            Ok(Progress::Ready(_)) => panic!("expired TLS admitted"),
            Ok(Progress::Pending(_)) => assert!(world.now() < end),
        }
    }
    let admission = crate::tls::Admission::new(u64::MAX);
    world.wall_offset(None, -86_400_000);
    world.advance(Duration::from_secs(299));
    assert!(!admission.expired(u64::MAX));
    world.advance(Duration::from_secs(1));
    assert!(admission.expired(u64::MAX));
    drop((exchange, listener));
    ring.shutdown().unwrap();
    drop(ring);
    world.assert_clean();
}
