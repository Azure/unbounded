// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

#[cfg(test)]
impl ControlChannel {}

#[cfg(test)]
pub(crate) mod tests {
    use super::*;
    use crate::control::{self, proto};

    fn context(node: u8, shard: u64, routing: &[u8]) -> Rc<Context> {
        context_with(node, shard, routing, |_| {})
    }
    fn context_with(
        node: u8,
        shard: u64,
        routing: &[u8],
        change: impl FnOnce(&mut proto::Snapshot),
    ) -> Rc<Context> {
        let (mut trust, mut config) = control::tests::fixture();
        trust.node = [node; 32];
        config.node = trust.node.to_vec();
        config.fabric = "fabric".into();
        let remote = if node == 2 { 3 } else { 2 };
        config.peers[0].id = NodeId::from_bytes(&[remote; 32]).unwrap().to_string();
        config.peers[0].fabric = config.fabric.clone();
        config.volumes[0].peers = vec![config.peers[0].id.clone()];
        config.volumes[0].topology = Some(proto::Topology {
            product: Some(proto::ProductTopology {
                left_factor: 1,
                right_factor: 2,
                members: vec!["02".repeat(32), "03".repeat(32)],
                roles: vec![0, 1],
                local_member: if node == 2 { 0 } else { 1 },
                candidate_width: 2,
                candidates: vec![0, 1, 1, 0],
            }),
            routing_algorithm: Some(1),
            epoch: 1,
            slot_count: 2,
            local_slots: vec![if node == 2 { 0 } else { 1 }],
        });
        change(&mut config);
        control::tests::scope_peers(&mut config);
        trust.universe = config.universe.as_slice().try_into().unwrap();
        let prepared = trust
            .prepare(proto::Configuration {
                contents: Some(proto::configuration::Contents::Snapshot(config)),
            })
            .unwrap();
        Rc::new(Context::new(Arc::new(prepared), "v1", shard, routing).unwrap())
    }
    pub(crate) fn offer(
        nonce: u8,
        challenge: [u8; 16],
        rail: u32,
        rails: u32,
        fabric: &str,
    ) -> Offer {
        Offer {
            version: 1,
            nonce: [nonce; 16],
            challenge,
            rail,
            rails,
            fabric: fabric.into(),
            reads: 1,
            endpoint: Endpoint {
                gid: [3; 16],
                qpn: 1,
                mtu: 1,
                ..Endpoint::default()
            },
        }
    }

    pub(crate) fn compiler_membership(
        ring: &mut Ring,
        a: Arc<Prepared>,
        b: Arc<Prepared>,
        aid: &crate::tls::PeerIdentity,
        bid: &crate::tls::PeerIdentity,
        ca: &crate::tls::tests::Authority,
        routing: &[u8],
    ) {
        use crate::tls::ExpectedPeer;
        struct Admission {
            server: Server,
            errors: Rc<RefCell<Vec<(io::ErrorKind, String)>>>,
        }
        impl http::Handler for Admission {
            type Task = io::Result<Task>;
            fn start(&mut self, request: http::Request) -> Self::Task {
                let task = self.server.start(request);
                if let Err(error) = &task {
                    self.errors
                        .borrow_mut()
                        .push((error.kind(), error.to_string()));
                }
                task
            }
            fn poll(
                &mut self,
                task: &mut Self::Task,
                ring: &mut Ring,
                budget: usize,
            ) -> io::Result<Progress<http::Completed>> {
                match task {
                    Ok(task) => self.server.poll_task(task, ring, budget),
                    Err(error) => Err(io::Error::new(error.kind(), error.to_string())),
                }
            }
        }
        let volume = &a.volumes()[0].config().id;
        let ac = Context::new(a.clone(), volume, 0, routing).unwrap();
        let bc = Rc::new(Context::new(b.clone(), volume, 0, routing).unwrap());
        let frame = ac.frame(
            false,
            &offer(1, [2; 16], 0, 1, a.fabric().unwrap().as_str()),
        );
        assert!(bc.validate(&frame, a.local_node()).is_ok());
        assert!(bc.authorize(Some(aid), a.local_node()).is_ok());
        let mut listener = http::Listener::bind(
            "127.0.0.1:0".parse().unwrap(),
            std::num::NonZeroU32::new(16).unwrap(),
        )
        .unwrap();
        listener.set_tls(
            ca.context(bid, false),
            ExpectedPeer::Universe(bid.universe.clone()),
        );
        let address = listener.local_addr().unwrap();
        let errors = Rc::new(RefCell::new(Vec::new()));
        let mut server = http::Server::new(
            listener,
            Admission {
                server: Server::new(
                    bc,
                    Rails::new(vec![None], 1).unwrap(),
                    8,
                    Duration::from_secs(5),
                )
                .unwrap(),
                errors: errors.clone(),
            },
            http::Config::default(),
        );
        for case in 0..3 {
            let mut identity = aid.clone();
            if case == 1 {
                identity.pod_uid = "wrong-process".into();
            }
            let mut hello = ac.frame(false, &frame.offer);
            if case == 2 {
                hello.volume[0] ^= 1;
            }
            let end = crate::environment::now() + Duration::from_secs(5);
            let mut request = client::Connection::new_tls(
                address,
                "localhost",
                &ca.context(&identity, false),
                ExpectedPeer::Identity(bid.clone()),
            )
            .unwrap()
            .head(
                client::Request::new("/object", &[(HEADER, &hello.encode())]).unwrap(),
                end,
            )
            .unwrap();
            loop {
                assert!(crate::environment::now() < end);
                ring.progress().unwrap();
                server.poll(ring, 64).unwrap();
                match request.poll(ring, 64) {
                    Err(_) => break,
                    Ok(Progress::Ready(_)) => panic!("no RNIC configured"),
                    _ => {}
                }
            }
            let (kind, message) = errors.borrow_mut().pop().expect("negotiation entered");
            match case {
                0 => assert_eq!(
                    kind,
                    io::ErrorKind::NotConnected,
                    "valid old member reached RNIC allocation: {message}"
                ),
                1 => assert_eq!(kind, io::ErrorKind::PermissionDenied),
                _ => assert!(message.contains("context mismatch")),
            }
        }
        server.shutdown(ring).unwrap();
    }
    fn control(kind: u8) -> Vec<u8> {
        let mut bytes = vec![0; control_wire::HEADER];
        control_wire::Frame {
            kind,
            session: [7; 16],
            ..Default::default()
        }
        .encode(&mut bytes);
        bytes
    }

    pub(crate) fn tls_channels(
        ao: &Offer,
        bo: &Offer,
        ring: &mut Ring,
        offload: bool,
    ) -> (
        (AuthenticatedOffer, ControlChannel),
        (AuthenticatedOffer, ControlChannel),
    ) {
        use crate::tls::{ExpectedPeer, tests::Authority};
        let a = context(2, 0, b"route");
        let b = context(3, 0, b"route");
        let ca = Authority::new();
        let aid = b
            .prepared
            .peer_identity(&a.prepared.local_node().to_string())
            .unwrap();
        let bid = a
            .prepared
            .peer_identity(&b.prepared.local_node().to_string())
            .unwrap();
        let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        let stream = std::net::TcpStream::connect(listener.local_addr().unwrap()).unwrap();
        let (remote, _) = listener.accept().unwrap();
        let mut at = client::TlsChannel::new(
            crate::uring::File::new(stream.into()),
            &ca.context(&aid, offload),
            ExpectedPeer::Identity(bid.clone()),
            false,
        )
        .unwrap();
        let mut bt = client::TlsChannel::new(
            crate::uring::File::new(remote.into()),
            &ca.context(&bid, offload),
            ExpectedPeer::Identity(aid.clone()),
            true,
        )
        .unwrap();
        let deadline = crate::environment::now() + Duration::from_secs(5);
        let (mut ad, mut bd) = (false, false);
        while !ad || !bd {
            ring.progress().unwrap();
            if !ad {
                ad = matches!(at.handshake(ring, deadline).unwrap(), Progress::Ready(()));
            }
            if !bd {
                bd = matches!(bt.handshake(ring, deadline).unwrap(), Progress::Ready(()));
            }
            assert!(crate::environment::now() < deadline);
        }
        if offload && std::env::var_os("RACER_REQUIRE_KTLS").is_some() {
            assert!(
                at.ktls_tx() && bt.ktls_tx(),
                "required native kTLS TX unavailable"
            );
        }
        let binding = binding(
            &a.frame(false, ao),
            &b.frame(true, bo),
            "/native",
            &a.prepared.config_snapshot().universe,
        );
        (
            (
                AuthenticatedOffer(bo.clone(), binding),
                ControlChannel::new(at, binding, &a, b.prepared.local_node()).unwrap(),
            ),
            (
                AuthenticatedOffer(ao.clone(), binding),
                ControlChannel::new(bt, binding, &b, a.prepared.local_node()).unwrap(),
            ),
        )
    }

    #[test]
    fn certificate_identity_is_exact_and_requires_current_volume_membership() {
        let a = context(2, 7, b"route");
        let node = NodeId::from_bytes(&[3; 32]).unwrap();
        let identity = a.prepared.peer_identity(&node.to_string()).unwrap();
        assert!(a.authorize(Some(&identity), node).is_ok());
        assert!(a.authorize(None, node).is_err());
        for field in [0, 1, 2] {
            let mut wrong = identity.clone();
            match field {
                0 => wrong.universe = "04".repeat(32),
                1 => wrong.node = "05".repeat(32),
                _ => wrong.pod_uid = "replacement-pod".into(),
            };
            assert!(a.authorize(Some(&wrong), node).is_err());
        }
        assert!(
            a.authorize(Some(&identity), NodeId::from_bytes(&[9; 32]).unwrap())
                .is_err()
        );
    }

    #[test]
    fn mixed_placement_negotiates_but_namespace_and_protocol_remain_exact() {
        let a = context(2, 7, b"route");
        let b = context_with(3, 7, b"route", |c| {
            c.volumes[0].topology.as_mut().unwrap().epoch = 2
        });
        let offer = offer(1, [2; 16], 0, 1, "fabric");
        let frame = b.frame(true, &offer);
        assert!(a.validate(&frame, b.prepared.local_node()).is_ok());
        for changed in [
            context_with(3, 7, b"route", |c| c.volumes[0].cache_generation += 1),
            context_with(3, 7, b"route", |c| c.universe = vec![9; 32]),
            context(3, 7, b"other-protocol"),
            context(3, 8, b"route"),
        ] {
            assert!(
                a.validate(&changed.frame(true, &offer), changed.prepared.local_node())
                    .is_err()
            );
        }
        assert!(a.current_placement());
        let next = context_with(2, 7, b"route", |c| {
            c.volumes[0].topology.as_mut().unwrap().epoch = 2
        });
        *a.authority.borrow_mut() = Some(next.prepared.clone());
        assert!(!a.current_placement());
        let node = b.prepared.local_node();
        let identity = a.prepared.peer_identity(&node.to_string()).unwrap();
        assert!(
            a.authorize(Some(&identity), node).is_ok(),
            "membership is independent of placement/session admission"
        );
    }

    #[test]
    fn strict_version_duplicate_hex_and_message_bounds() {
        let a = context(2, 0, b"");
        let o = offer(1, [2; 16], 2, 3, "fabric");
        let bytes = o.encode();
        assert_eq!(Offer::decode(&bytes).unwrap().encode(), bytes);
        for n in 0..bytes.len() {
            assert!(Offer::decode(&bytes[..n]).is_err());
        }
        for version in [0u32, 2, 3] {
            let mut bad = bytes.clone();
            bad[..4].copy_from_slice(&version.to_be_bytes());
            assert!(Offer::decode(&bad).is_err());
        }
        for (offset, value) in [(64, 0), (65, 1)] {
            let mut bad = bytes.clone();
            bad[offset] = value;
            assert!(Offer::decode(&bad).is_err());
        }
        let wire = a.frame(false, &o).encode();
        assert!(parse_fields([(HEADER, wire.as_bytes())].into_iter()).is_ok());
        assert!(
            parse_fields(
                [(HEADER, wire.as_bytes()), ("x-racer-rdma", wire.as_bytes())].into_iter()
            )
            .is_err()
        );
        for bad in [
            String::new(),
            "0".into(),
            "00".repeat(PREFIX + peer_identity::MAX_OFFER_LEN + 1),
            wire.to_uppercase(),
            format!("02{}", &wire[2..]),
            format!("{wire}00"),
            format!(" {wire}"),
            format!("{wire},{wire}"),
        ] {
            assert!(Frame::decode(bad.as_bytes()).is_err());
        }
        for n in 0..wire.len() {
            assert!(Frame::decode(&wire.as_bytes()[..n]).is_err());
        }
        let max = a
            .frame(false, &offer(1, [5; 16], 255, 256, &"f".repeat(256)))
            .encode();
        assert_eq!(max.len(), 2 * (PREFIX + peer_identity::MAX_OFFER_LEN));
        assert!(Frame::decode(max.as_bytes()).is_ok());
        for bad in [
            offer(1, [0; 16], 0, 1, "fabric"),
            offer(1, [5; 16], 256, 257, "fabric"),
            offer(1, [5; 16], 0, 1, "bad fabric"),
        ] {
            assert!(Frame::decode(a.frame(false, &bad).encode().as_bytes()).is_err());
        }
    }

    #[test]
    fn sparse_catalog_selection_and_inbound_topology_eligibility() {
        for shard in 0..100 {
            for (n, m) in [(4, 4), (3, 7)] {
                let (a, b) = rails_for_shard(shard, n, m).unwrap();
                assert_eq!((a, b), (shard as usize % n, shard as usize % m));
                assert_eq!(rails_for_shard(shard, m, n), Some((b, a)));
            }
        }
        assert_eq!(rails_for_shard(1, 0, 3), None);
        let c = context(3, 7, b"route");
        assert!(
            c.prepared
                .eligible_node_for_volume("v1", NodeId::from_bytes(&[2; 32]).unwrap())
                .is_some()
        );
        for (len, total) in [(0, 0), (1, 2), (257, 257)] {
            assert!(Rails::new(vec![None; len], total).is_err());
        }
        let rails = Rails::new(vec![None; 3], 3).unwrap();
        assert_eq!(rails.total(), 3);
        assert_eq!(
            rails.prepare(&c, [5; 16]).err().unwrap().kind(),
            io::ErrorKind::NotConnected
        );
        for (rail, challenge, fabric, valid) in [
            (3, 5, "fabric", true),
            (0, 5, "fabric", false),
            (3, 6, "fabric", false),
            (3, 5, "other", false),
        ] {
            assert_eq!(
                validate_offer(
                    &c,
                    &rails,
                    &offer(1, [challenge; 16], rail, 4, fabric),
                    [5; 16]
                )
                .is_ok(),
                valid
            );
        }
    }

    #[test]
    fn tls_framed_channel_real_socket_bidirectional_persistent() {
        use crate::tls::{ExpectedPeer, tests::Authority};
        let Some(mut ring) = control::tests::ring() else {
            return;
        };
        for offload in [false, true] {
            let a = context(2, 0, b"route");
            let b = context(3, 0, b"route");
            let ca = Authority::new();
            let aid = b
                .prepared
                .peer_identity(&a.prepared.local_node().to_string())
                .unwrap();
            let bid = a
                .prepared
                .peer_identity(&b.prepared.local_node().to_string())
                .unwrap();
            let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
            let stream = std::net::TcpStream::connect(listener.local_addr().unwrap()).unwrap();
            let (remote, _) = listener.accept().unwrap();
            let mut at = client::TlsChannel::new(
                crate::uring::File::new(stream.into()),
                &ca.context(&aid, offload),
                ExpectedPeer::Identity(bid.clone()),
                false,
            )
            .unwrap();
            let mut bt = client::TlsChannel::new(
                crate::uring::File::new(remote.into()),
                &ca.context(&bid, offload),
                ExpectedPeer::Identity(aid.clone()),
                true,
            )
            .unwrap();
            let deadline = crate::environment::now() + Duration::from_secs(5);
            let (mut ad, mut bd) = (false, false);
            while !ad || !bd {
                ring.progress().unwrap();
                if !ad {
                    ad = matches!(
                        at.handshake(&mut ring, deadline).unwrap(),
                        Progress::Ready(())
                    );
                }
                if !bd {
                    bd = matches!(
                        bt.handshake(&mut ring, deadline).unwrap(),
                        Progress::Ready(())
                    );
                }
                assert!(crate::environment::now() < deadline);
            }
            let binding = [11; 32];
            if offload && std::env::var_os("RACER_REQUIRE_KTLS").is_some() {
                assert!(
                    at.ktls_tx() && bt.ktls_tx(),
                    "required native kTLS TX unavailable"
                );
            }
            let mut ac = ControlChannel::new(at, binding, &a, b.prepared.local_node()).unwrap();
            let mut bc = ControlChannel::new(bt, binding, &b, a.prepared.local_node()).unwrap();
            for round in 0..3 {
                ac.enqueue(round, &control(5)).unwrap();
                bc.enqueue(round + 10, &control(6)).unwrap();
                let (mut ar, mut br, mut sent_a, mut sent_b) = (false, false, false, false);
                while !(ar && br && sent_a && sent_b) {
                    ring.progress().unwrap();
                    ac.poll(&mut ring, 2).unwrap();
                    bc.poll(&mut ring, 2).unwrap();
                    if let Some(bytes) = ac.take_received() {
                        assert_eq!(bytes, control(6));
                        ar = true;
                    }
                    if let Some(bytes) = bc.take_received() {
                        assert_eq!(bytes, control(5));
                        br = true;
                    }
                    if let Some(tag) = ac.take_sent() {
                        assert_eq!(tag, round);
                        sent_a = true;
                    }
                    if let Some(tag) = bc.take_sent() {
                        assert_eq!(tag, round + 10);
                        sent_b = true;
                    }
                    assert!(crate::environment::now() < deadline, "TLS controls stalled");
                }
            }
            ac.close();
            assert!(!ac.healthy());
            loop {
                ring.progress().unwrap();
                if bc.poll(&mut ring, 2).is_err() {
                    break;
                }
                assert!(crate::environment::now() < deadline, "TLS EOF not observed");
            }
            bc.close();
        }
        ring.shutdown().unwrap();
    }
}
