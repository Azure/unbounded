// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

#[cfg(test)]
impl ControlChannel {
    pub(crate) fn test_revoke(&mut self) {
        *self.membership.as_ref().unwrap().0.authority.borrow_mut() = None;
        assert!(!self.admitting());
        assert!(self.healthy());
    }
    pub(crate) fn test_rotate(&mut self) {
        let identity = self.membership.as_ref().unwrap().1.clone();
        let ca = crate::tls::tests::Authority::new();
        self.provider = Some(Provider::for_test(
            identity.clone(),
            ca.context(&identity, false),
        ));
        self.revision = 0;
        assert!(!self.admitting());
        assert!(self.healthy());
    }
    pub(crate) fn test_descriptor(&self) -> (Vec<u8>, [u8; 32], u32) {
        let context = &self.membership.as_ref().unwrap().0;
        let routing = context
            .prepared
            .routing_for_volume(&context.volume_id)
            .unwrap();
        let cursor = routing.start("/rotation");
        let mut bytes = b"RF04".to_vec();
        bytes.extend_from_slice(&10000u32.to_le_bytes());
        bytes.extend_from_slice(b"RF03");
        bytes.extend_from_slice(&cursor.encode());
        bytes.extend_from_slice(b"RF05\0/rotation");
        (bytes, cursor.identity, routing.destination(&cursor))
    }
}

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

    pub(crate) fn wall_authentication(world: &crate::simulation::World) {
        use crate::simulation::history::{Transition, require};
        let _receiver = world.scoped_node(Some(1));
        let context = context(2, 0, b"clock");
        let node = NodeId::from_bytes(&[3; 32]).unwrap();
        let identity = context.prepared.peer_identity(&node.to_string()).unwrap();
        let mut channel = ControlChannel::simulated([3; 32]);
        channel.membership = Some(((*context).clone(), identity, node));
        channel.deadline = world.now() + Duration::from_secs(121);
        let began = world.now();
        for offset in [60_000, 61_000, -60_000, -61_000, 0] {
            world.wall_offset(Some(1), offset);
            require(
                channel.admitting(),
                "authentication.tls-wall",
                "wall step changed TLS membership admission",
            );
            require(
                world.now() == began,
                "clock.monotonic-isolation",
                "wall step advanced request deadline",
            );
            world.observation(Transition::AuthenticationWallChecked {
                offset,
                accepted: true,
            });
        }
        *context.authority.borrow_mut() = None;
        for offset in [86_400_000, -86_400_000, 0] {
            world.wall_offset(Some(1), offset);
            require(
                !channel.admitting() && channel.healthy(),
                "authentication.membership-clock",
                "wall step restored revoked admission or aborted draining controls",
            );
            world.observation(Transition::AuthenticationMembershipRetained { offset });
        }
        channel.enqueue(1, &control(6)).unwrap();
        world.advance(Duration::from_secs(120));
        require(
            channel.healthy(),
            "authentication.channel-clock",
            "channel retired before its original monotonic deadline",
        );
        world.advance(Duration::from_secs(1));
        require(
            !channel.healthy() && channel.enqueue(2, &control(6)).is_err(),
            "authentication.channel-expiry",
            "expired TLS channel admitted controls",
        );
        *context.authority.borrow_mut() = Some(context.prepared.clone());
        require(
            !channel.admitting(),
            "authentication.expired-channel",
            "membership restoration resurrected an expired TLS channel",
        );
        world.observation(Transition::AuthenticationChannelExpired { elapsed: 121 });
    }

    pub(crate) fn seeded_channel_pressure(world: &crate::simulation::World) -> Vec<(u64, bool)> {
        let _scope = world.enter();
        let mut channel = ControlChannel::simulated([3; 32]);
        let mut outcomes = Vec::new();
        for _ in 0..128 {
            let mut bytes = [0; 8];
            world.random(&mut bytes);
            let tag = u64::from_le_bytes(bytes);
            outcomes.push((tag, channel.enqueue(tag, &control(5)).is_ok()));
        }
        assert_eq!(outcomes.iter().filter(|(_, ok)| *ok).count(), CHANNEL_DEPTH);
        world.advance(Duration::from_secs(3600));
        assert!(!channel.healthy());
        assert!(channel.enqueue(999, &control(5)).is_err());
        let mut fresh = ControlChannel::simulated([4; 32]);
        assert!(fresh.enqueue(999, &control(5)).is_ok());
        outcomes
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
        let world = crate::simulation::World::new(27030);
        let _scope = world.enter();
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
        assert_eq!(channel.drain.get(), Some(channel.deadline));
        world.advance(Duration::from_secs(31));
        assert!(
            channel.healthy(),
            "replacement must not create an earlier transport expiry"
        );
        channel.drain.set(Some(crate::environment::now()));
        assert!(!channel.healthy());
        assert!(channel.enqueue(2, &control(6)).is_err());
        let mut aging = ControlChannel::simulated([5; 32]);
        aging.deadline = crate::environment::now() + DRAIN;
        assert!(!aging.admitting());
        assert!(aging.healthy());
        aging.deadline = crate::environment::now();
        assert!(!aging.healthy());
        let mut config = TransportConfig::default();
        config.timeout = DRAIN;
        assert!(config.validate().is_ok());
        config.timeout += Duration::from_nanos(1);
        assert!(
            config.validate().is_err(),
            "RPC deadlines must fit the admission grace"
        );
    }

    #[test]
    fn live_membership_rechecks_removed_and_replaced_pod_without_aborting_controls() {
        let context = context(2, 0, b"route");
        let node = NodeId::from_bytes(&[3; 32]).unwrap();
        let identity = context.prepared.peer_identity(&node.to_string()).unwrap();
        let mut channel = ControlChannel::simulated([3; 32]);
        channel.membership = Some(((*context).clone(), identity, node));
        assert!(channel.admitting());
        let replacement = context_with(2, 0, b"route", |s| {
            s.peers[0].pod_uid = "replacement".into()
        });
        *context.authority.borrow_mut() = Some(replacement.prepared.clone());
        assert!(!channel.admitting());
        assert!(channel.healthy());
        channel.enqueue(1, &control(6)).unwrap();
        *context.authority.borrow_mut() = None;
        assert!(!channel.admitting());
        assert!(channel.healthy());
        *context.authority.borrow_mut() = Some(context.prepared.clone());
        assert!(channel.admitting());
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
    fn confirmed_queued_connection_accepts_bidirectional_requests_before_collection() {
        let (_a, ca, _b, cb, _) = confirmation_pair();
        ca.begin_confirmation(true, crate::environment::now() + Duration::from_secs(5))
            .unwrap();
        ca.test_pump(&cb, false);
        cb.test_pump(&ca, false);
        let context = context(3, 0, b"confirmation");
        let mut server = Server::new(
            context.clone(),
            Rails::new(vec![None], 1).unwrap(),
            1,
            Duration::from_secs(5),
        )
        .unwrap();
        let deadline = crate::environment::now() + Duration::from_secs(5);
        server.leases.set(1);
        server.store.borrow_mut().completed.push_back(Queued {
            established: Established {
                connection: cb,
                context,
                peer: NodeId::from_bytes(&[2; 32]).unwrap(),
                confirmation_deadline: Some(deadline),
            },
            deadline,
            _lease: Lease(server.leases.clone()),
        });
        {
            let store = server.store.borrow();
            let cb = &store.completed[0].established.connection;
            assert_eq!(server.reserved(), 1);
            for (from, to) in [(&ca, cb), (cb, &ca)] {
                let ticket = from.test_deliver_request(to);
                assert!(to.authenticated_received() && to.is_healthy());
                let request = to.next_request().unwrap().unwrap();
                assert_eq!(request.metadata, b"invalid descriptor");
                drop((ticket, request));
            }
        }
        assert!(
            server
                .take_completed(crate::environment::now())
                .unwrap()
                .connection
                .is_confirmed()
        );
        assert_eq!(server.reserved(), 0);
    }

    fn confirmation_pair() -> (
        rdma::Transport,
        rdma::Connection,
        rdma::Transport,
        rdma::Connection,
        crate::buffers::WorkerPool,
    ) {
        let pool = crate::buffers::io_test_pool(2);
        let a = rdma::test_transport_config(&pool, 1, 1);
        let b = rdma::test_transport_config(&pool, 1, 1);
        let aq = a.prepare_for_fabric("fabric", [5; 16], 0, 1).unwrap();
        let bq = b.prepare_for_fabric("fabric", [5; 16], 0, 1).unwrap();
        let ((ao, ac), (bo, bc)) = test_channels(aq.offer(), bq.offer());
        let ca = aq.connect_authenticated(ao, ac, 0).unwrap();
        let cb = bq.connect_authenticated(bo, bc, 0).unwrap();
        cb.begin_confirmation(false, crate::environment::now() + Duration::from_secs(5))
            .unwrap();
        (a, ca, b, cb, pool)
    }

    pub(super) fn confirmation_admission() {
        use crate::simulation::history::{Transition, require};
        let world = crate::simulation::current().unwrap();
        let (a, ca, b, cb, _) = confirmation_pair();
        // Ready authenticated the responder, but no Confirm has been delivered.
        assert!(cb.is_authenticated());
        assert!(!cb.is_confirmed());
        world.observation(Transition::UnconfirmedRequestAttempt { confirmed: false });
        let attempt = cb.request([1; 32], 4, b"before-confirm");
        require(
            matches!(&attempt, Err(error) if error.kind() == io::ErrorKind::WouldBlock),
            "session.confirmation-admission",
            "application request admitted before Confirm",
        );
        ca.begin_confirmation(true, crate::environment::now() + Duration::from_secs(5))
            .unwrap();
        ca.test_pump(&cb, false);
        cb.test_pump(&ca, false);
        assert!(ca.is_confirmed() && cb.is_confirmed());
        let request = cb.request([1; 32], 4, b"after-confirm").unwrap();
        world.observation(Transition::ConfirmedRequestAdmitted { confirmed: true });
        cb.test_pump(&ca, false);
        let received = ca.next_request().unwrap().unwrap();
        assert_eq!(received.metadata, b"after-confirm");
        drop((received, request, ca, cb));
        a.shutdown().unwrap();
        b.shutdown().unwrap();
        assert_eq!(a.test_invariants(), (0, 0, 0));
        assert_eq!(b.test_invariants(), (0, 0, 0));
    }

    #[test]
    fn unconfirmed_session_mutant_requires_admission_oracle() {
        use crate::simulation::history::{Failure, Mutant};
        for mutant in [None, Some(Mutant::UnconfirmedSessionAdmission)] {
            let world = crate::simulation::World::new(19);
            let _scope = world.enter();
            world.enable_scheduler();
            world.mutant(mutant);
            let result =
                std::panic::catch_unwind(std::panic::AssertUnwindSafe(confirmation_admission));
            if mutant.is_some() {
                assert_eq!(
                    result
                        .unwrap_err()
                        .downcast_ref::<Failure>()
                        .unwrap()
                        .oracle,
                    "session.confirmation-admission"
                );
            } else {
                result.unwrap();
            }
        }
    }

    #[test]
    fn confirmation_handles_early_ack_and_idle_then_bidirectional_requests() {
        let world = crate::simulation::World::new(710);
        let _scope = world.enter();
        let (a, ca, b, cb, pool) = confirmation_pair();
        let before_a = a.test_invariants();
        let before_b = b.test_invariants();
        ca.begin_confirmation(true, crate::environment::now() + Duration::from_secs(5))
            .unwrap();
        assert_eq!(
            ca.request([1; 32], 4, &[]).err().unwrap().kind(),
            io::ErrorKind::WouldBlock
        );
        let (_, aq) = ca.test_endpoint();
        let (_, bq) = cb.test_endpoint();
        let confirm = aq.posts()[0];
        assert_eq!(confirm.kind, 5);
        assert!(aq.effect(&bq, confirm, false).unwrap());
        for recv in bq.receives() {
            bq.complete(recv, 0).unwrap();
        }
        b.test_progress(32).unwrap();
        assert!(cb.next_request().unwrap().is_none());
        assert!(!cb.is_confirmed()); // ACK is still NIC-owned.
        let ack = bq.posts()[0];
        assert_eq!(ack.kind, 6);
        assert!(bq.effect(&aq, ack, false).unwrap());
        for recv in aq.receives() {
            aq.complete(recv, 0).unwrap();
        }
        a.test_progress(32).unwrap();
        assert!(!ca.is_confirmed()); // Received ACK before local Confirm CQE.
        aq.complete(confirm, 0).unwrap();
        bq.complete(ack, 0).unwrap();
        assert!(a.test_progress(32).unwrap().runnable);
        assert!(b.test_progress(32).unwrap().runnable);
        assert!(ca.is_confirmed() && cb.is_confirmed());
        assert_eq!(a.test_invariants(), before_a);
        assert_eq!(b.test_invariants(), before_b);
        world.advance(Duration::from_secs(30));
        a.test_progress(32).unwrap();
        b.test_progress(32).unwrap();
        assert!(ca.is_healthy() && cb.is_healthy());
        // A fresh signed RPC in each direction must still validate the sequence
        // after Ready/Confirm/ACK and must not be confused with request zero.
        let mut ta = ca.request([1; 32], 4, b"a").unwrap();
        let tb = cb.request([2; 32], 4, b"b").unwrap();
        ca.test_pump(&cb, false);
        cb.test_pump(&ca, false);
        let request_a = cb.next_request().unwrap().unwrap();
        let request_b = ca.next_request().unwrap().unwrap();
        assert_eq!(request_a.metadata, b"a");
        assert_eq!(request_b.metadata, b"b");
        // Exercise the complete BIND -> grant -> READ -> ACK -> INV lifecycle
        // after idle, at depth one, with real registered-pool-shaped storage.
        let key = crate::buffers::Key::new([1; 32]);
        let mut source = pool.stage(key).unwrap();
        source.as_mut_slice()[..4].copy_from_slice(b"data");
        let crc = crate::allocator::crc64(b"data");
        cb.respond(request_a, source.publish_checked(4, crc).unwrap())
            .unwrap();
        cb.test_pump(&ca, false);
        cb.test_pump(&ca, false);
        let grant = ca.take_grant(&mut ta).unwrap().unwrap();
        let mut read = ca.read(grant, pool.stage(key).unwrap()).unwrap();
        ca.test_pump(&cb, false);
        ca.test_pump(&cb, false);
        cb.test_pump(&ca, false);
        assert_eq!(
            ca.take_read(&mut read).unwrap().unwrap().as_slice(),
            b"data"
        );
        drop((ta, tb, request_b));
    }

    #[test]
    fn confirmation_queue_pressure_loss_and_replay_are_bounded() {
        for loss in [5, 6, 7] {
            let world = crate::simulation::World::new(711 + loss);
            let _scope = world.enter();
            let (a, ca, b, cb, _) = confirmation_pair();
            a.test_faults(1, false, false, false);
            ca.begin_confirmation(true, crate::environment::now() + Duration::from_secs(5))
                .unwrap();
            let pending = a.test_observe();
            assert_eq!(pending.pending, vec![false]);
            assert!(!ca.is_confirmed());
            a.test_faults(0, false, false, false);
            a.test_progress(32).unwrap();
            let wire = a.test_observe().sends[0].1.clone();
            if loss != 5 {
                ca.test_pump(&cb, false);
                assert!(!cb.is_confirmed());
            }
            if loss == 7 {
                // Replay is rejected even though no application RPC has arrived.
                assert!(b.test_inject(&wire).is_err());
                assert!(!cb.is_healthy());
            }
            world.advance(Duration::from_secs(6));
            a.test_progress(32).unwrap();
            b.test_progress(32).unwrap();
            assert!(!ca.is_healthy() && !cb.is_healthy());
            assert_eq!(a.test_invariants().2, 0);
            assert_eq!(b.test_invariants().2, 0);
        }
    }

    #[test]
    fn confirmation_rejects_authenticated_wrong_kind_and_noncanonical_fields() {
        for (offset, value) in [
            (4, 6),
            (8, 1),
            (31, 1),
            (32, 1),
            (67, 1),
            (75, 1),
            (83, 1),
            (87, 1),
            (89, 1),
        ] {
            let (a, ca, _b, cb, _) = confirmation_pair();
            a.test_edit_control(move |body| body[offset] = value);
            ca.begin_confirmation(true, crate::environment::now() + Duration::from_secs(5))
                .unwrap();
            ca.test_pump(&cb, false);
            assert!(!cb.is_healthy(), "accepted changed control byte {offset}");
        }
    }

    #[test]
    fn confirmation_ack_pressure_and_cancel_retain_control_until_quiescence() {
        let world = crate::simulation::World::new(719);
        let _scope = world.enter();
        let (a, ca, b, cb, _) = confirmation_pair();
        ca.begin_confirmation(true, crate::environment::now() + Duration::from_secs(5))
            .unwrap();
        b.test_faults(1, false, false, false);
        ca.test_pump(&cb, false);
        assert_eq!(b.test_observe().pending, vec![false]);
        assert!(!ca.is_confirmed() && !cb.is_confirmed());
        b.test_faults(0, false, false, false);
        b.test_progress(32).unwrap();
        let before = b.test_observe().sends;
        assert_eq!(before.len(), 1);
        b.test_block_destroy(true);
        drop(cb);
        b.test_progress(32).unwrap();
        assert_eq!(b.test_observe().sends, before);
        assert_eq!(b.test_observe().qps, 1);
        b.test_block_destroy(false);
        world.advance(Duration::from_millis(100));
        b.shutdown().unwrap();
        assert_eq!(b.test_invariants(), (0, 0, 0));
        // The initiator still requires the ACK; local SEND completion is not proof.
        assert!(!ca.is_confirmed());
        drop(ca);
        a.shutdown().unwrap();
    }

    #[test]
    fn first_request_can_arrive_before_confirmation_ack_send_completion() {
        let (a, ca, b, cb, _) = confirmation_pair();
        ca.begin_confirmation(true, Instant::now() + Duration::from_secs(5))
            .unwrap();
        ca.test_pump(&cb, false);
        let (_, aq) = ca.test_endpoint();
        let (_, bq) = cb.test_endpoint();
        let ack = bq.posts()[0];
        assert!(bq.effect(&aq, ack, false).unwrap());
        for recv in aq.receives() {
            aq.complete(recv, 0).unwrap();
        }
        a.test_progress(32).unwrap();
        assert!(ca.is_confirmed());
        assert!(!cb.is_confirmed());
        let owned_ack = b.test_observe().sends;
        let ticket = ca.request([1; 32], 4, b"first").unwrap();
        ca.test_pump(&cb, false);
        let request = cb.next_request().unwrap().unwrap();
        assert_eq!(request.metadata, b"first");
        assert_eq!(b.test_observe().sends, owned_ack);
        bq.complete(ack, 0).unwrap();
        b.test_progress(32).unwrap();
        assert!(cb.is_confirmed());
        drop((request, ticket));
    }

    #[test]
    fn abandoned_initiator_after_ack_replaces_at_full_capacity_and_reconnects() {
        let Some(mut ring) = control::tests::ring() else {
            return;
        };
        let mut fixture = ReplacementFixture::new(&ring);
        let (client, old) = fixture.ack_without_receipt(&mut ring);
        let old_qp = old.test_endpoint().1;
        drop(client);
        assert_eq!(fixture.at.test_observe().qps, 0);
        assert_eq!(fixture.bt.test_observe().qps, 1);

        // An offer alone cannot evict a live TLS session. Until its source sees
        // EOF, the abandoned predecessor still occupies the only responder QP.
        fixture.bt.test_block_destroy(true);
        fixture.rejected_client(&mut ring, false);
        assert!(old.is_healthy());
        let end = Instant::now() + Duration::from_secs(5);
        while old.is_healthy() {
            fixture.tick(&mut ring, false, true);
            assert!(
                Instant::now() < end,
                "abandoned TLS channel did not observe EOF"
            );
        }
        for _ in 0..3 {
            fixture.rejected_client(&mut ring, true);
            assert_eq!(fixture.bt.test_observe().qps, 1);
        }
        // Release the injected destroy block and drive ordinary source cleanup,
        // without closing the predecessor by hand or shutting down the transport.
        fixture.bt.test_block_destroy(false);
        while fixture.bt.test_observe().qps != 0 {
            fixture.tick(&mut ring, false, true);
            assert!(Instant::now() < end, "bounded QP cleanup retry stalled");
        }
        let mut next = fixture.start();
        let mut outbound = None;
        let mut inbound = None;
        while outbound.is_none()
            || !inbound
                .as_ref()
                .is_some_and(|e: &Established| e.connection.is_confirmed())
        {
            fixture.tick(&mut ring, true, true);
            if outbound.is_none() {
                if let Progress::Ready(done) = next.poll(&mut ring, 16).unwrap() {
                    outbound = Some(done);
                }
            }
            if inbound.is_none() {
                inbound = fixture
                    .http
                    .handler_mut()
                    .server
                    .take_completed(Instant::now());
            }
            assert!(
                Instant::now() < end,
                "fresh replacement confirmation stalled"
            );
        }
        let outbound = outbound.unwrap();
        let inbound = inbound.unwrap();
        assert!(outbound.connection.is_confirmed() && outbound.connection.authenticated_received());
        assert!(inbound.connection.authenticated_received());
        assert!(!old_qp.same(&inbound.connection.test_endpoint().1));
        assert!(!old.is_healthy());
        assert_eq!(fixture.bt.test_observe().qps, 1);
        assert_eq!(fixture.http.handler().server.reserved(), 0);
        drop((outbound, inbound, next, old, old_qp));
        fixture.shutdown(&mut ring);
        ring.shutdown().unwrap();
    }

    #[test]
    fn replacement_tls_identity_binding_and_membership_never_evict() {
        let Some(mut ring) = control::tests::ring() else {
            return;
        };
        let mut fixture = ReplacementFixture::new(&ring);
        let (mut client, old) = fixture.ack_without_receipt(&mut ring);
        let end = Instant::now() + Duration::from_secs(5);
        let live = loop {
            fixture.tick(&mut ring, true, true);
            if let Progress::Ready(done) = client.poll(&mut ring, 16).unwrap() {
                break done;
            }
            assert!(Instant::now() < end);
        };
        let old_qp = old.test_endpoint().1;
        for attack in [
            "universe",
            "node",
            "pod",
            "missing certificate",
            "removed membership",
            "replaced membership",
            "claimed node",
            "volume",
            "shard",
            "route",
            "generation",
            "fabric",
            "rail",
            "zero challenge",
        ] {
            let mut identity = fixture.aid.clone();
            let mut frame = fixture.a.frame(false, &offer(1, [5; 16], 0, 1, "fabric"));
            match attack {
                "universe" => identity.universe = "09".repeat(32),
                "node" => identity.node = "09".repeat(32),
                "pod" => identity.pod_uid = "replacement".into(),
                "removed membership" => *fixture.b.authority.borrow_mut() = None,
                "replaced membership" => {
                    *fixture.b.authority.borrow_mut() = Some(
                        context_with(3, 0, b"route", |s| {
                            s.peers[0].pod_uid = "replacement".into();
                        })
                        .prepared
                        .clone(),
                    );
                }
                "claimed node" => frame.node = NodeId::from_bytes(&[9; 32]).unwrap(),
                "volume" => frame.volume[0] ^= 1,
                "shard" => frame.shard += 1,
                "route" => frame.routing[0] ^= 1,
                "generation" => {
                    frame = context_with(2, 0, b"route", |s| {
                        s.volumes[0].cache_generation += 1;
                    })
                    .frame(false, &frame.offer)
                }
                "fabric" => frame.offer.fabric = "foreign".into(),
                "rail" => {
                    frame.offer.rail = 1;
                    frame.offer.rails = 2;
                }
                "zero challenge" => frame.offer.challenge = [0; 16],
                _ => (),
            }
            let tls = if attack == "missing certificate" {
                crate::tls::TlsContext::bootstrap(&fixture.ca.bundle()).unwrap()
            } else {
                fixture.ca.context(&identity, false)
            };
            for _ in 0..2 {
                let starts = fixture.http.handler().starts;
                let errors = fixture.http.handler().errors.len();
                fixture.reject_offer(&mut ring, &tls, &frame);
                let handshake_rejection = matches!(attack, "universe" | "missing certificate");
                assert_eq!(
                    fixture.http.handler().starts - starts,
                    usize::from(!handshake_rejection),
                    "{attack}"
                );
                assert_eq!(
                    fixture.http.handler().errors.len() - errors,
                    usize::from(!handshake_rejection),
                    "{attack}"
                );
                if !handshake_rejection {
                    // Capacity alone would mask a missing authorization or binding
                    // check. Require the offer's actual validation failure.
                    let expected = match attack {
                        "node" | "pod" | "replaced membership" => (
                            io::ErrorKind::PermissionDenied,
                            "TLS peer certificate membership mismatch",
                        ),
                        "removed membership" => {
                            (io::ErrorKind::InvalidData, "no receive authority")
                        }
                        "claimed node" => (io::ErrorKind::InvalidData, "ineligible TLS node"),
                        "fabric" | "rail" => (
                            io::ErrorKind::InvalidData,
                            "offer fabric/challenge/rail mismatch",
                        ),
                        "zero challenge" => (io::ErrorKind::InvalidData, "missing offer challenge"),
                        _ => (
                            io::ErrorKind::InvalidData,
                            "negotiation identity/context mismatch",
                        ),
                    };
                    let error = fixture.http.handler().errors.last().unwrap();
                    assert_eq!((error.0, error.1.as_str()), expected, "{attack}");
                }
                assert!(old.is_healthy() && old.is_confirmed(), "{attack}");
                assert!(
                    live.connection.is_healthy() && live.connection.is_confirmed(),
                    "{attack}"
                );
                assert!(old_qp.same(&old.test_endpoint().1), "{attack}");
                assert_eq!(fixture.bt.test_observe().qps, 1, "{attack}");
            }
            *fixture.b.authority.borrow_mut() = Some(fixture.b.prepared.clone());
        }
        // The preserved session must still carry authenticated controls both ways.
        let mut owners = Vec::new();
        for (from, to) in [
            (&live.connection, old.as_ref()),
            (old.as_ref(), &live.connection),
        ] {
            let ticket = from.request([1; 32], 4, b"still-live").unwrap();
            let end = Instant::now() + Duration::from_secs(5);
            let request = loop {
                fixture.tick(&mut ring, true, true);
                if let Some(request) = to.next_request().unwrap() {
                    break request;
                }
                assert!(
                    Instant::now() < end,
                    "preserved session stopped carrying controls"
                );
            };
            assert_eq!(request.metadata, b"still-live");
            owners.push((ticket, request));
        }
        assert!(live.connection.is_healthy() && old.is_healthy());
        drop(owners);
        drop((client, live, old, old_qp));
        fixture.shutdown(&mut ring);
        ring.shutdown().unwrap();
    }

    include!("replacement.rs");

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
