// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

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
            routing_algorithm: None,
            epoch: 1,
            slot_count: 2,
            local_slots: vec![if node == 2 { 0 } else { 1 }],
            neighbors: vec![proto::SlotPeer {
                slot: if node == 2 { 1 } else { 0 },
                peer: config.peers[0].id.clone(),
            }],
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
    pub(super) fn offer(
        nonce: u8,
        challenge: [u8; 16],
        rail: u32,
        rails: u32,
        fabric: &str,
    ) -> Offer {
        Offer {
            version: 2,
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
    fn ingest(channel: &mut ControlChannel, bytes: &[u8]) -> io::Result<()> {
        for byte in bytes {
            channel.input.push(*byte);
            channel.decode_input()?;
        }
        Ok(())
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
    fn binding_covers_both_offers_target_universe_route_and_generation() {
        let a = context(2, 7, b"route");
        let b = context(3, 7, b"route");
        let hello = a.frame(false, &offer(1, [5; 16], 0, 1, "fabric"));
        let reply = b.frame(true, &offer(2, [5; 16], 0, 1, "fabric"));
        b.validate(&hello, a.prepared.local_node()).unwrap();
        let original = binding(&hello, &reply, "/object", &[1; 32]);
        assert_ne!(original, binding(&hello, &reply, "/other", &[1; 32]));
        assert_ne!(original, binding(&hello, &reply, "/object", &[2; 32]));
        for changed in [
            context(3, 8, b"route"),
            context(3, 7, b"new-route"),
            context_with(3, 7, b"route", |s| s.volumes[0].cache_generation += 1),
            context_with(3, 7, b"route", |s| {
                s.volumes[0].topology.as_mut().unwrap().epoch += 1
            }),
        ] {
            assert!(changed.validate(&hello, a.prepared.local_node()).is_err());
            assert_ne!(
                original,
                binding(
                    &hello,
                    &changed.frame(true, &reply.offer),
                    "/object",
                    &[1; 32]
                )
            );
        }
        let mut altered = reply;
        altered.offer.endpoint.qpn += 1;
        assert_ne!(original, binding(&hello, &altered, "/object", &[1; 32]));
        let mut channel = ControlChannel::simulated(original);
        assert!(channel.matches_transport(&original).is_ok());
        assert!(channel.matches_transport(&[0; 32]).is_err());
        channel.close();
        assert!(!channel.healthy());
        assert!(channel.matches_transport(&original).is_err());
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
        for version in [0u32, 1, 3] {
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
    fn framed_control_partial_io_bounds_binding_and_backpressure() {
        let binding = [9; 32];
        let mut sender = ControlChannel::simulated(binding);
        let bytes = control(5);
        sender.enqueue(7, &bytes).unwrap();
        assert!(sender.take_sent().is_none());
        let wire = sender.outgoing.front().unwrap().bytes.clone();
        let mut receiver = ControlChannel::simulated(binding);
        for byte in &wire[..wire.len() - 1] {
            ingest(&mut receiver, &[*byte]).unwrap();
            assert!(receiver.take_received().is_none());
        }
        ingest(&mut receiver, &wire[wire.len() - 1..]).unwrap();
        assert_eq!(receiver.take_received().unwrap(), bytes);
        assert!(receiver.input.is_empty());
        assert_eq!(receiver.expected, 4);
        for len in [0u32, 31, 32 + 111, 32 + MAX_CONTROL as u32 + 1, u32::MAX] {
            let mut receiver = ControlChannel::simulated(binding);
            assert!(ingest(&mut receiver, &len.to_be_bytes()).is_err());
            assert!(receiver.input.len() <= 4);
        }
        let mut foreign = ControlChannel::simulated([8; 32]);
        assert!(ingest(&mut foreign, &wire).is_err());
        for tag in 1..CHANNEL_DEPTH {
            sender.enqueue(tag as u64, &bytes).unwrap();
        }
        assert_eq!(
            sender.enqueue(999, &bytes).unwrap_err().kind(),
            io::ErrorKind::WouldBlock
        );
        assert_eq!(sender.outgoing.len(), CHANNEL_DEPTH);
        assert!(sender.enqueue(1, &vec![0; MAX_CONTROL + 1]).is_err());
        let mut malformed = wire.clone();
        malformed[36] = b'X';
        assert!(ingest(&mut ControlChannel::simulated(binding), &malformed).is_err());
    }

    #[test]
    fn channel_rotation_stops_new_admission_but_drains_existing_controls() {
        use crate::tls::tests::Authority;
        let context = context(2, 0, b"route");
        let identity = context
            .prepared
            .peer_identity(&NodeId::from_bytes(&[3; 32]).unwrap().to_string())
            .unwrap();
        let ca = Authority::new();
        let provider = Provider::for_test(identity.clone(), ca.context(&identity, false));
        let mut channel = ControlChannel::simulated([4; 32]);
        channel.revision = provider.current().revision;
        channel.provider = Some(provider);
        assert!(channel.admitting());
        // Captured revision is immutable on a live channel. A newer installed
        // context starts a bounded drain while queued terminal controls survive.
        channel.revision = 0;
        assert!(!channel.admitting());
        assert!(channel.healthy());
        channel.enqueue(1, &control(6)).unwrap();
        assert_eq!(channel.outgoing.len(), 1);
        assert!(channel.drain.get().is_some());
        channel.drain.set(Some(crate::environment::now()));
        assert!(!channel.healthy());
        assert!(channel.enqueue(2, &control(6)).is_err());
        let mut aging = ControlChannel::simulated([5; 32]);
        aging.deadline = crate::environment::now() + DRAIN;
        assert!(!aging.admitting());
        assert!(aging.healthy());
        aging.deadline = crate::environment::now();
        assert!(!aging.healthy());
    }

    #[test]
    fn connected_unconfirmed_queue_expires_and_releases_capacity() {
        let a = context(2, 0, b"route");
        let b = context(3, 0, b"route");
        let rails = Rails::new(vec![None], 1).unwrap();
        assert!(Context::new(a.prepared.clone(), "absent", 0, b"").is_err());
        assert!(Context::new(a.prepared.clone(), "v1", 0, &vec![0; 1025]).is_err());
        for (cap, time) in [
            (0, Duration::from_secs(60)),
            (1, Duration::ZERO),
            (1, Duration::from_secs(61)),
        ] {
            assert!(Server::new(a.clone(), rails.clone(), cap, time).is_err());
        }
        let mut server = Server::new(b.clone(), rails, 1, Duration::from_secs(60)).unwrap();
        let deadline = crate::environment::now() + Duration::from_secs(1);
        let pair = rdma::tests::rdma_ownership_corpus::Pair::new(1);
        server.leases.set(1);
        server.store.borrow_mut().completed.push_back(Queued {
            established: Established {
                connection: pair.bc,
                context: b,
                peer: a.prepared.local_node(),
                confirmation_deadline: Some(deadline),
            },
            deadline,
            _lease: Lease(server.leases.clone()),
        });
        assert_eq!(server.reserved(), 1);
        assert_eq!(
            server.poll(deadline - Duration::from_nanos(1)).deadline,
            Some(deadline)
        );
        assert_eq!(server.poll(deadline).deadline, None);
        assert_eq!(server.reserved(), 0);
        assert!(server.take_completed(deadline).is_none());
    }

    #[test]
    fn confirmation_controls_preserve_dma_ownership_without_verbs_send_recv() {
        let pair = rdma::tests::rdma_ownership_corpus::Pair::new(1);
        let end = crate::environment::now() + Duration::from_secs(5);
        pair.bc.begin_confirmation(false, end).unwrap();
        pair.ac.begin_confirmation(true, end).unwrap();
        assert!(!pair.ac.is_confirmed());
        assert!(!pair.bc.is_confirmed());
        for _ in 0..4 {
            pair.ac.test_pump(&pair.bc, false);
            pair.bc.test_pump(&pair.ac, false);
        }
        assert!(pair.ac.is_confirmed());
        assert!(pair.bc.is_confirmed());
        assert!(pair.ac.authenticated_received());
        assert!(pair.bc.authenticated_received());
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

    #[test]
    fn tls_http_offer_takeover_confirms_on_same_socket() {
        use crate::tls::{ExpectedPeer, tests::Authority};
        struct Handler(Server);
        impl http::Handler for Handler {
            type Task = io::Result<Task>;
            fn start(&mut self, request: http::Request) -> Self::Task {
                self.0.start(request)
            }
            fn poll(
                &mut self,
                task: &mut Self::Task,
                ring: &mut Ring,
                budget: usize,
            ) -> io::Result<Progress<http::Completed>> {
                self.0.poll_task(
                    task.as_mut().map_err(|e| io::Error::other(e.to_string()))?,
                    ring,
                    budget,
                )
            }
        }
        let Some(mut ring) = control::tests::ring() else {
            return;
        };
        let ca = Authority::new();
        let base_a = context(2, 0, b"route");
        let base_b = context(3, 0, b"route");
        let aid = base_b
            .prepared
            .peer_identity(&base_a.prepared.local_node().to_string())
            .unwrap();
        let bid = base_a
            .prepared
            .peer_identity(&base_b.prepared.local_node().to_string())
            .unwrap();
        let mut listener = http::Listener::bind(
            "127.0.0.1:0".parse().unwrap(),
            std::num::NonZeroU32::new(16).unwrap(),
        )
        .unwrap();
        listener.set_tls(
            ca.context(&bid, false),
            ExpectedPeer::Universe(bid.universe.clone()),
        );
        let address = listener.local_addr().unwrap();
        let a = context_with(2, 0, b"route", |s| {
            s.peers[0].http_address = address.to_string()
        });
        let provider = Provider::for_test(aid.clone(), ca.context(&aid, false));
        let a = Rc::new(
            Rc::try_unwrap(a)
                .ok()
                .unwrap()
                .with_credentials(Some(provider)),
        );
        let b = base_b;
        let at = rdma::test_transport_config(ring.pool(), 2, 4);
        let bt = rdma::test_transport_config(ring.pool(), 2, 4);
        let ar = Rails::new(vec![Some(at.clone())], 1).unwrap();
        let br = Rails::new(vec![Some(bt.clone())], 1).unwrap();
        let server = Server::new(b.clone(), br, 2, Duration::from_secs(5)).unwrap();
        let mut http = http::Server::new(listener, Handler(server), http::Config::default());
        let mut client = Client::start(
            a,
            ar,
            &b.prepared.local_node().to_string(),
            "/object",
            Duration::from_secs(5),
        )
        .unwrap();
        let mut asource = at.test_source();
        let mut bsource = bt.test_source();
        let mut inbound = None;
        let mut outbound = None;
        let end = crate::environment::now() + Duration::from_secs(5);
        loop {
            ring.progress().unwrap();
            http.poll(&mut ring, 4).unwrap();
            if inbound.is_none() {
                inbound = http
                    .handler_mut()
                    .0
                    .take_completed(crate::environment::now());
            }
            if outbound.is_none() {
                if let Progress::Ready(done) = client.poll(&mut ring, 4).unwrap() {
                    outbound = Some(done);
                }
            }
            crate::uring::CompletionSource::poll(&mut asource, &mut ring, 4).unwrap();
            crate::uring::CompletionSource::poll(&mut bsource, &mut ring, 4).unwrap();
            if outbound.is_some()
                && inbound
                    .as_ref()
                    .is_some_and(|e| e.connection.is_confirmed())
            {
                break;
            }
            assert!(
                crate::environment::now() < end,
                "offer takeover confirmation stalled"
            );
        }
        assert!(outbound.unwrap().connection.authenticated_received());
        assert!(inbound.unwrap().connection.authenticated_received());
        drop(http);
        drop(client);
        at.shutdown().unwrap();
        bt.shutdown().unwrap();
        ring.shutdown().unwrap();
    }
}
