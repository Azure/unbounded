// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

#[cfg(test)]
mod tests {
    use super::*;
    use crate::control::{self, proto};
    type Reply = (auth::Responder, auth::Reply);
    type Hello = (auth::Initiator, auth::Hello);
    type Exchange = (auth::Initiator, auth::Responder, auth::Reply);
    type Http = http::Server<HttpResponder>;
    fn rejected<T, E>(result: Result<T, E>) {
        assert!(result.is_err());
    }
    fn validate(sender: &Context, receiver: &Context, kind: Kind, payload: Vec<u8>) -> Frame {
        let parsed = Frame::decode(sender.frame(kind, payload).encode().as_bytes()).unwrap();
        receiver
            .validate(&parsed, sender.prepared.local_node())
            .unwrap();
        parsed
    }

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
        let contents = Some(proto::configuration::Contents::Snapshot(config));
        let prepared = trust.prepare(proto::Configuration { contents }).unwrap();
        Rc::new(Context::new(Arc::new(prepared), "v1", shard, routing).unwrap())
    }
    pub(super) fn offer(
        nonce: u8,
        challenge: [u8; 16],
        rail: u32,
        rails: u32,
        fabric: &str,
    ) -> rdma::Offer {
        let offer = Offer {
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
        };
        let parsed = Offer::decode(&offer.encode()).unwrap();
        assert_eq!(parsed.encode(), offer.encode());
        assert_eq!(parsed.challenge(), challenge);
        assert_eq!(parsed.rail_index(), rail as usize);
        assert_eq!(parsed.rail_count(), rails as usize);
        assert_eq!(parsed.fabric(), fabric);
        parsed
    }
    fn begin(a: &Context, b: &Context, target: &str) -> Exchange {
        let ao = offer(1, [5; 16], 0, 1, "fabric");
        let bo = offer(2, [5; 16], 0, 1, "fabric");
        let (initiator, hello) = start(a, b, target, &ao);
        let parsed = validate(a, b, Kind::Hello, hello.encode().to_vec());
        let hello = auth::Hello::decode(&parsed.payload).unwrap();
        let (responder, reply) = accept(a, b, target, &bo, hello);
        (initiator, responder, reply)
    }
    fn start(a: &Context, b: &Context, target: &str, offer: &Offer) -> Hello {
        auth::Initiator::start(
            a.snapshot().clone(),
            a.peers(b.prepared.local_node(), true, target).unwrap(),
            Some(offer),
            MAX_TIMEOUT,
        )
        .unwrap()
    }
    fn accept(a: &Context, b: &Context, target: &str, offer: &Offer, hello: auth::Hello) -> Reply {
        auth::Responder::accept(
            b.snapshot().clone(),
            b.peers(a.prepared.local_node(), false, target).unwrap(),
            hello,
            Some(offer),
            MAX_TIMEOUT,
        )
        .unwrap()
    }

    #[test]
    fn two_head_protocol_preserves_session_for_bidirectional_envelopes() {
        let a = context(2, 17, b"route-generation-9");
        let b = context(3, 17, b"route-generation-9");
        let target = "/any/object?not=a-reserved-path";
        let (initiator, responder, reply) = begin(&a, &b, target);
        let parsed = validate(&b, &a, Kind::Reply, reply.encode().to_vec());
        let reply = auth::Reply::decode(&parsed.payload).unwrap();
        let (mut sa, finish) = initiator.finish(reply).unwrap();
        let (_, other, _) = begin(&a, &b, target);
        rejected(other.finish(auth::Finish::decode(finish.encode()).unwrap()));
        let parsed = validate(&a, &b, Kind::Finish, finish.encode().to_vec());
        let finish = auth::Finish::decode(&parsed.payload).unwrap();
        let mut sb = responder.finish(finish).unwrap();
        assert!(sa.take_offer(a.snapshot()).unwrap().is_some());
        assert!(sa.take_offer(a.snapshot()).is_err());
        assert!(sb.take_offer(b.snapshot()).unwrap().is_some());
        let ready = sign_ready(&mut sb, b.snapshot(), [5; 16]).unwrap();
        for n in 0..ready.len() {
            let mut changed = ready.clone();
            changed[n] ^= 1;
            rejected(verify_ready(&mut sa, a.snapshot(), [5; 16], &changed));
        }
        rejected(verify_ready(&mut sb, b.snapshot(), [5; 16], &ready));
        let (other, _, reply) = begin(&a, &b, "/");
        let (mut other, _) = other.finish(reply).unwrap();
        rejected(verify_ready(&mut other, a.snapshot(), [5; 16], &ready));
        let parsed = validate(&b, &a, Kind::Ready, ready.clone());
        verify_ready(&mut sa, a.snapshot(), [5; 16], &parsed.payload).unwrap();
        assert!(verify_ready(&mut sa, a.snapshot(), [5; 16], &ready).is_err());
        // Ready advanced exactly the reverse direction. Keep these same sessions.
        let control = auth::Control::new(42, b"RPC".to_vec()).unwrap();
        let request = sa.sign(a.snapshot(), control).unwrap();
        assert_eq!(sb.verify(b.snapshot(), request).unwrap().body(), b"RPC");
        let control = auth::Control::new(42, b"grant".to_vec()).unwrap();
        let response = sb.sign(b.snapshot(), control).unwrap();
        assert_eq!(sa.verify(a.snapshot(), response).unwrap().body(), b"grant");
        let ready = sign_ready(&mut sb, b.snapshot(), [5; 16]).unwrap();
        rejected(verify_ready(&mut sa, a.snapshot(), [6; 16], &ready));
    }

    #[test]
    fn context_and_offer_tampering_cannot_complete_authentication() {
        let a = context(2, 7, b"route");
        let different_epoch =
            |s: &mut proto::Snapshot| s.volumes[0].topology.as_mut().unwrap().epoch += 1;
        for (b, target) in [
            (context(3, 8, b"route"), "/a"),
            (context(3, 7, b"other"), "/a"),
            (context(3, 7, b"route"), "/b"),
            (context_with(3, 7, b"route", different_epoch), "/a"),
        ] {
            let ao = offer(1, [5; 16], 0, 1, "fabric");
            let bo = offer(2, [5; 16], 0, 1, "fabric");
            let (i, h) = start(&a, &b, "/a", &ao);
            let (_, r) = accept(&a, &b, target, &bo, h);
            assert!(i.finish(r).is_err());
        }
        let variants = |node| {
            [
                context(node, 7, b"route"),
                context_with(node, 7, b"route", different_epoch),
            ]
        };
        let peers = variants(2);
        let remotes = variants(3);
        assert_ne!(peers[0].routing, peers[1].routing);
        for (i, a) in peers.iter().enumerate() {
            for (j, b) in remotes.iter().enumerate() {
                let frame = a.frame(Kind::Hello, vec![]);
                assert_eq!(b.validate(&frame, a.prepared.local_node()).is_ok(), i == j);
            }
        }
    }

    #[test]
    fn strict_version_duplicate_hex_and_message_bounds() {
        let a = context(2, 0, b"");
        let b = context(3, 0, b"");
        let (_, _, reply) = begin(&a, &b, "/");
        let bytes = offer(1, [2; 16], 2, 3, "fabric").encode();
        for version in [0u32, 1, 3] {
            let mut bad = bytes.clone();
            bad[..4].copy_from_slice(&version.to_be_bytes());
            rejected(Offer::decode(&bad));
        }
        for n in 0..bytes.len() {
            rejected(Offer::decode(&bytes[..n]));
        }
        for (offset, value) in [(64, 0), (65, 1)] {
            let mut bad = bytes.clone();
            bad[offset] = value;
            rejected(Offer::decode(&bad));
        }
        let wire = b.frame(Kind::Reply, reply.encode().to_vec()).encode();
        assert!(parse_fields([(HEADER, wire.as_bytes())].into_iter()).is_ok());
        let duplicate = [(HEADER, wire.as_bytes()), ("x-racer-rdma", wire.as_bytes())];
        rejected(parse_fields(duplicate.into_iter()));
        assert!(parse_fields([("X-Unrelated", wire.as_bytes())].into_iter()).is_err());
        for bad in [
            String::new(),
            "0".into(),
            "00".repeat(MAX_FRAME + 1),
            wire.to_uppercase(),
            format!("01{}", &wire[2..]),
            format!("01ff{}", &wire[4..]),
            format!("{wire}00"),
            format!(" {wire}"),
            format!("{wire},{wire}"),
        ] {
            assert!(Frame::decode(bad.as_bytes()).is_err());
        }
        for n in 0..wire.len() {
            if n == 2 * (PREFIX + 128) {
                // A truncated offered Reply can have the replacement shape,
                // but cannot carry a valid signature of an empty-offer transcript.
                let parsed = Frame::decode(&wire.as_bytes()[..n]).unwrap();
                let (initiator, _, _) = begin(&a, &b, "/");
                rejected(initiator.finish(auth::Reply::decode(&parsed.payload).unwrap()));
            } else {
                rejected(Frame::decode(&wire.as_bytes()[..n]));
            }
        }
        let max_offer = offer(1, [5; 16], 255, 256, &"f".repeat(256));
        let payload = [vec![1; 32], max_offer.encode(), vec![1; 96]].concat();
        let max = a.frame(Kind::Reply, payload).encode();
        assert_eq!(max.len(), MAX_FRAME * 2);
        assert!(Frame::decode(max.as_bytes()).is_ok());
        assert!(client::Request::new("/any", &[(HEADER, &max)]).is_ok());
        assert!(http::ResponseHead::new(200, Some(0), &[(HEADER, max.as_bytes())]).is_ok());
        for bad in [
            offer(1, [0; 16], 0, 1, "fabric"),
            offer(1, [5; 16], 256, 257, "fabric"),
            offer(1, [5; 16], 0, 1, "bad fabric"),
        ] {
            let payload = [vec![1; 64], bad.encode()].concat();
            let frame = a.frame(Kind::Hello, payload).encode();
            rejected(Frame::decode(frame.as_bytes()));
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
        let context = context(3, 7, b"route");
        let remote = NodeId::from_bytes(&[2; 32]).unwrap();
        let eligible = context.prepared.eligible_node_for_volume("v1", remote);
        assert!(eligible.is_some());
        let remote_id = remote.to_string();
        let eligible = context.prepared.eligible_peer_for_volume("v1", &remote_id);
        assert!(eligible.is_some());
        for (len, total) in [(0, 0), (1, 2), (257, 257)] {
            rejected(Rails::new(vec![None; len], total));
        }
        let rails = Rails::new(vec![None; 3], 3).unwrap();
        assert_eq!(rails.total(), 3);
        let error = rails.prepare(&context, [5; 16]).err().unwrap();
        assert_eq!(error.kind(), io::ErrorKind::NotConnected);
        for (rail, challenge, fabric, valid) in [
            (3, 5, "fabric", true),
            (0, 5, "fabric", false),
            (3, 6, "fabric", false),
            (3, 5, "other", false),
        ] {
            let offer = offer(1, [challenge; 16], rail, 4, fabric);
            let result = validate_offer(&context, &rails, &offer, [5; 16]);
            assert_eq!(result.is_ok(), valid);
        }
    }

    #[test]
    fn connected_unconfirmed_queue_expires_and_releases_capacity() {
        let a = context(2, 0, b"route");
        let b = context(3, 0, b"route");
        let rails = Rails::new(vec![None], 1).unwrap();
        rejected(Context::new(a.prepared.clone(), "absent", 0, b""));
        let oversized = vec![0; auth::MAX_ROUTING_CONTEXT + 1];
        rejected(Context::new(a.prepared.clone(), "v1", 0, &oversized));
        assert!(Arc::ptr_eq(a.prepared(), &a.prepared));
        for (cap, time) in [
            (0, MAX_TIMEOUT),
            (1, Duration::ZERO),
            (1, MAX_TIMEOUT + Duration::from_secs(1)),
        ] {
            rejected(Server::new(a.clone(), rails.clone(), cap, time));
        }
        let mut server = Server::new(b.clone(), rails, 1, MAX_TIMEOUT).unwrap();
        assert_eq!(server.poll(Instant::now()).deadline, None);
        assert!(server.take_completed(Instant::now()).is_none());
        let deadline = Instant::now() + Duration::from_secs(1);
        // Full HTTP authentication/Ready is exercised by the admission test.
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
        let before = deadline - Duration::from_nanos(1);
        assert!(fresh(deadline, before).is_ok());
        assert!(fresh(deadline, deadline).is_err());
        assert_eq!(server.poll(before).deadline, Some(deadline));
        assert_eq!(server.poll(deadline).deadline, None);
        assert_eq!(server.reserved(), 0);
        assert!(server.take_completed(deadline).is_none());
    }

    struct HttpResponder {
        server: Server,
        errors: Vec<io::ErrorKind>,
        abandon: bool,
    }
    impl HttpResponder {
        fn completed(&mut self) -> Option<Established> {
            self.server.take_completed(Instant::now())
        }
        fn reserved(&self) -> usize {
            self.server.reserved()
        }
    }
    fn listener() -> http::Listener {
        http::Listener::bind(
            "127.0.0.1:0".parse().unwrap(),
            std::num::NonZeroU32::new(8).unwrap(),
        )
        .unwrap()
    }
    fn rails(ring: &Ring) -> Rails {
        Rails::new(vec![Some(rdma::test_transport(ring.pool()))], 1).unwrap()
    }
    fn responder(
        listener: http::Listener,
        context: Rc<Context>,
        rails: Rails,
        abandon: bool,
    ) -> Http {
        let handler = HttpResponder {
            server: Server::new(context, rails, 1, MAX_TIMEOUT).unwrap(),
            errors: vec![],
            abandon,
        };
        http::Server::new(listener, handler, http::Config::default())
    }
    fn tick(ring: &mut Ring, server: &mut Http) {
        ring.progress().unwrap();
        server.poll(ring, 64).unwrap();
    }
    fn until(ring: &mut Ring, server: &mut Http, ready: impl Fn(&HttpResponder) -> bool) {
        let end = Instant::now() + Duration::from_secs(3);
        while !ready(server.handler()) {
            tick(ring, server);
            server.handler_mut().server.poll(Instant::now());
            assert!(Instant::now() < end, "negotiation stalled");
        }
    }
    impl http::Handler for HttpResponder {
        type Task = io::Result<Task>;
        fn start(&mut self, request: http::Request) -> Self::Task {
            let task = self.server.start(request);
            if let Ok(Task {
                after: Some(AfterSend::Ready(ready)),
                ..
            }) = &task
            {
                // Check before the HTTP task's first poll can submit Ready's SEND.
                assert!(ready.established.connection.is_authenticated());
                assert!(ready.established.connection.is_healthy());
            }
            if let Err(error) = &task {
                self.errors.push(error.kind());
            }
            if self.abandon {
                drop(task);
                Err(invalid("injected scheduler abandonment"))
            } else {
                task
            }
        }
        fn poll(
            &mut self,
            task: &mut Self::Task,
            ring: &mut Ring,
            budget: usize,
        ) -> io::Result<Progress<http::Completed>> {
            match task {
                Ok(task) => self.server.poll_task(task, ring, budget),
                Err(_) => Err(invalid("rejected negotiation")),
            }
        }
    }

    fn exchange(
        ring: &mut Ring,
        server: &mut Http,
        tcp: client::Connection,
        target: &str,
        frame: &Frame,
    ) -> io::Result<client::HeadResponse> {
        let wire = frame.encode();
        let end = Instant::now() + Duration::from_secs(3);
        let mut request = tcp.head(client::Request::new(target, &[(HEADER, &wire)])?, end)?;
        loop {
            ring.progress()?;
            server.poll(ring, 64)?;
            match request.poll(ring, 64)? {
                Progress::Ready(response) => return Ok(response),
                Progress::Pending(_) => assert!(Instant::now() < end, "HTTP negotiation stalled"),
            }
            std::thread::yield_now();
        }
    }

    #[test]
    fn ready_completion_accepts_authenticated_rdma_before_manager_admission() {
        let Some(mut ring) = control::tests::ring() else {
            return;
        };
        let listener = listener();
        let address = listener.local_addr().unwrap();
        let a = context_with(2, 0, b"route", |s| {
            s.peers[0].http_address = address.to_string()
        });
        let b = context(3, 0, b"route");
        let ar = rails(&ring);
        let br = rails(&ring);
        let mut server = responder(listener, b.clone(), br.clone(), false);
        let mut client = Client::start(
            a,
            ar.clone(),
            &b.prepared.local_node().to_string(),
            "/ordinary?target=1",
            MAX_TIMEOUT,
        )
        .unwrap();
        let end = Instant::now() + Duration::from_secs(3);
        loop {
            tick(&mut ring, &mut server);
            assert!(matches!(
                client.poll(&mut ring, 64).unwrap(),
                Progress::Pending(_)
            ));
            if matches!(client.state, ClientState::Confirming(_)) {
                break;
            }
            assert!(Instant::now() < end, "HTTP Ready not received");
        }
        // HTTP success alone is not sufficient for outbound admission.
        assert!(matches!(
            client.poll(&mut ring, 64).unwrap(),
            Progress::Pending(_)
        ));
        let established = loop {
            tick(&mut ring, &mut server);
            test_confirmations(&ar);
            test_confirmations(&br);
            if let Progress::Ready(established) = client.poll(&mut ring, 64).unwrap() {
                break established;
            }
            assert!(Instant::now() < end, "Ready not received");
        };
        // The client has verified ConfirmAck. Collect HTTP send completion but
        // deliberately never call take_completed / Manager::poll or admission.
        until(&mut ring, &mut server, |h| {
            !h.server.store.borrow().completed.is_empty()
        });
        {
            let store = server.handler().server.store.borrow();
            let queued = &store.completed[0].established;
            assert_eq!(server.handler().server.reserved(), 1);
            assert!(queued.confirmation_deadline.is_some());
            assert!(queued.connection.is_confirmed());
            // Simulated NIC delivers a signed request through the real RECV/control
            // verification path, while the server still owns the undrained result.
            let mut owners = Vec::new();
            for (from, to) in [
                (&established.connection, &queued.connection),
                (&queued.connection, &established.connection),
            ] {
                let ticket = from.test_deliver_request(to);
                assert!(to.authenticated_received() && to.is_healthy());
                let request = to.next_request().unwrap().unwrap();
                assert_eq!(request.metadata, b"invalid descriptor");
                owners.push((ticket, request));
            }
        }
        let inbound = server.handler_mut().completed().unwrap();
        assert!(inbound.connection.is_authenticated());
        assert!(inbound.connection.authenticated_received());
        drop((inbound, established, client));
        server.shutdown(&mut ring).unwrap();
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
        let ac = context(2, 0, b"confirmation");
        let bc = context(3, 0, b"confirmation");
        let aq = a.prepare_for_fabric("fabric", [5; 16], 0, 1).unwrap();
        let bq = b.prepare_for_fabric("fabric", [5; 16], 0, 1).unwrap();
        let (initiator, hello) = start(&ac, &bc, "/idle", aq.offer());
        let (responder, reply) = accept(&ac, &bc, "/idle", bq.offer(), hello);
        let (mut sa, finish) = initiator.finish(reply).unwrap();
        let mut sb = responder.finish(finish).unwrap();
        let ready = sign_ready(&mut sb, bc.snapshot(), [5; 16]).unwrap();
        verify_ready(&mut sa, ac.snapshot(), [5; 16], &ready).unwrap();
        let ca = aq
            .connect_authenticated(sa, ac.snapshot().clone(), 0)
            .unwrap();
        let cb = bq
            .connect_authenticated(sb, bc.snapshot().clone(), 0)
            .unwrap();
        cb.begin_confirmation(false, crate::environment::now() + Duration::from_secs(5))
            .unwrap();
        (a, ca, b, cb, pool)
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
            assert_eq!(pending.pending, vec![true]);
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
                b.test_inject(&wire).unwrap();
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
        assert_eq!(b.test_observe().pending, vec![true]);
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
        let listener = listener();
        let address = listener.local_addr().unwrap();
        let a = context_with(2, 0, b"route", |s| {
            s.peers[0].http_address = address.to_string()
        });
        let b = context(3, 0, b"route");
        let at = rdma::test_transport_config(ring.pool(), 1, 1);
        let bt = rdma::test_transport_config(ring.pool(), 1, 1);
        let ar = Rails::new(vec![Some(at.clone())], 1).unwrap();
        let br = Rails::new(vec![Some(bt.clone())], 1).unwrap();
        let mut server = responder(listener, b.clone(), br.clone(), false);
        let start_client = || {
            Client::start(
                a.clone(),
                ar.clone(),
                &b.prepared.local_node().to_string(),
                "/replace",
                MAX_TIMEOUT,
            )
            .unwrap()
        };
        let mut client = start_client();
        let end = Instant::now() + Duration::from_secs(3);
        loop {
            tick(&mut ring, &mut server);
            assert!(matches!(
                client.poll(&mut ring, 64).unwrap(),
                Progress::Pending(_)
            ));
            if matches!(client.state, ClientState::Confirming(_)) {
                break;
            }
            assert!(Instant::now() < end);
        }
        until(&mut ring, &mut server, |h| {
            !h.server.store.borrow().completed.is_empty()
        });
        let old = Rc::new(server.handler_mut().completed().unwrap().connection);
        test_confirmations(&ar);
        test_confirmations(&br);
        assert!(old.is_confirmed()); // ACK SEND retired; initiator RECV is still queued.
        drop(client);
        assert!(old.is_healthy());
        assert!(bt.prepare_for_fabric("fabric", [9; 16], 0, 1).is_err());
        server.handler_mut().server = Server::new(b.clone(), br.clone(), 1, MAX_TIMEOUT)
            .unwrap()
            .replacing(old.clone());

        // Authenticating the replacement needs no spare responder QP. Even a
        // blocked destroy keeps exactly one QP and cannot grow a retired list.
        bt.test_block_destroy(true);
        let mut replacement = start_client();
        loop {
            tick(&mut ring, &mut server);
            match replacement.poll(&mut ring, 64) {
                Err(e) => {
                    assert_eq!(e.kind(), io::ErrorKind::ConnectionAborted);
                    break;
                }
                Ok(Progress::Pending(_)) => (),
                Ok(Progress::Ready(_)) => panic!("replacement must retry normal negotiation"),
            }
            assert!(Instant::now() < end);
        }
        assert!(!old.is_healthy());
        assert_eq!(bt.test_observe().qps, 1);
        assert!(bt.prepare_for_fabric("fabric", [9; 16], 0, 1).is_err());
        bt.test_block_destroy(false);
        // Drive the existing bounded cleanup retry, without a source shutdown.
        while bt.test_observe().qps != 0 {
            bt.test_progress(32).unwrap();
            assert!(Instant::now() < end);
        }
        until(&mut ring, &mut server, |h| h.reserved() == 0);
        server.handler_mut().server = Server::new(b.clone(), br.clone(), 1, MAX_TIMEOUT).unwrap();
        let mut next = start_client();
        let established = loop {
            tick(&mut ring, &mut server);
            test_confirmations(&ar);
            test_confirmations(&br);
            if let Progress::Ready(e) = next.poll(&mut ring, 64).unwrap() {
                break e;
            }
            assert!(Instant::now() < end);
        };
        assert!(established.connection.is_confirmed());
        until(&mut ring, &mut server, |h| {
            !h.server.store.borrow().completed.is_empty()
        });
        assert!(
            server
                .handler_mut()
                .completed()
                .unwrap()
                .connection
                .is_confirmed()
        );
        assert_eq!(bt.test_observe().qps, 0); // dropped the newly collected owner
        server.shutdown(&mut ring).unwrap();
    }

    #[test]
    fn replacement_hello_bad_finish_replay_and_wrong_tcp_never_evict() {
        let Some(mut ring) = control::tests::ring() else {
            return;
        };
        for attack in 0..5 {
            let (at, ca, bt, cb, _) = confirmation_pair();
            ca.begin_confirmation(true, Instant::now() + Duration::from_secs(5))
                .unwrap();
            ca.test_pump(&cb, false);
            cb.test_pump(&ca, false);
            let old = Rc::new(cb);
            let a = context(2, 0, b"route");
            let b = context(3, 0, b"route");
            let listener = listener();
            let address = listener.local_addr().unwrap();
            let rails = Rails::new(vec![Some(bt.clone())], 1).unwrap();
            let mut server = responder(listener, b.clone(), rails, false);
            server.handler_mut().server.replacement = Some(old.clone());
            let offer = offer(1, [5; 16], 0, 1, "fabric");
            let (initiator, hello) = start(&a, &b, "/replace", &offer);
            let hello = a.frame(Kind::Hello, hello.encode().to_vec());
            let tcp = || client::Connection::new(address, "localhost").unwrap();
            let reply = exchange(&mut ring, &mut server, tcp(), "/replace", &hello).unwrap();
            until(&mut ring, &mut server, |h| {
                h.reserved() == 1 && !h.server.store.borrow().pending.is_empty()
            });
            assert!(old.is_healthy());
            assert_eq!(bt.test_observe().qps, 1);
            // A second Hello is bounded by the single lease, without eviction.
            assert!(exchange(&mut ring, &mut server, tcp(), "/replace", &hello).is_err());
            let payload = parse_fields(reply.headers().iter()).unwrap().payload;
            assert_eq!(payload.len(), 128);
            let (_, finish) = initiator
                .finish(auth::Reply::decode(&payload).unwrap())
                .unwrap();
            let mut bytes = finish.encode().to_vec();
            let continuation = reply.recycle().unwrap();
            if attack == 0 {
                drop(continuation);
            } else {
                if attack == 1 {
                    bytes[0] ^= 1;
                }
                if attack == 4 {
                    let (other, _, reply) = begin(&a, &b, "/replace");
                    bytes = other.finish(reply).unwrap().1.encode().to_vec();
                }
                let finish = a.frame(Kind::Finish, bytes);
                let target = if attack == 2 { "/other" } else { "/replace" };
                let connection = if attack == 3 {
                    drop(continuation);
                    tcp()
                } else {
                    continuation
                };
                assert!(exchange(&mut ring, &mut server, connection, target, &finish).is_err());
            }
            until(&mut ring, &mut server, |h| h.reserved() == 0);
            assert!(old.is_healthy());
            assert_eq!(bt.test_observe().qps, 1);
            server.shutdown(&mut ring).unwrap();
            drop((ca, old));
            at.shutdown().unwrap();
            bt.shutdown().unwrap();
        }
    }

    #[test]
    fn http_responder_rejects_continuation_attacks_and_reclaims_abandoned_handshakes() {
        let Some(mut ring) = control::tests::ring() else {
            return;
        };
        let a = context(2, 0, b"route");
        let b = context(3, 0, b"route");
        // Each case starts from a real HTTP Hello/Reply with one reserved QP.
        for case in 0..9 {
            let listener = listener();
            let address = listener.local_addr().unwrap();
            let mut server = responder(listener, b.clone(), rails(&ring), case == 6);
            let ao = offer(1, [5; 16], 0, 1, "fabric");
            let (initiator, hello) = start(&a, &b, "/object", &ao);
            let hello = a.frame(Kind::Hello, hello.encode().to_vec());
            let connect = || client::Connection::new(address, "localhost").unwrap();
            let response = exchange(&mut ring, &mut server, connect(), "/object", &hello);
            if case == 6 {
                assert!(response.is_err());
                assert_eq!(server.handler().server.reserved(), 0);
                server.shutdown(&mut ring).unwrap();
                continue;
            }
            let response = response.unwrap();
            until(&mut ring, &mut server, |h| {
                !h.server.store.borrow().pending.is_empty()
            });
            assert_eq!(server.handler().server.reserved(), 1);
            let reply = parse_fields(response.headers().iter()).unwrap();
            let reply = auth::Reply::decode(&reply.payload).unwrap();
            let (_, finish) = initiator.finish(reply).unwrap();
            let finish = a.frame(Kind::Finish, finish.encode().to_vec());
            let tcp = response.recycle().unwrap();
            if case == 5 {
                drop(tcp);
            } else {
                let mut tcp = tcp;
                if case == 1 || case == 4 {
                    let frame = if case == 1 { &finish } else { &hello };
                    rejected(exchange(
                        &mut ring,
                        &mut server,
                        connect(),
                        "/object",
                        frame,
                    ));
                    assert_eq!(server.handler().reserved(), 1);
                    if case == 1 {
                        drop(tcp);
                        until(&mut ring, &mut server, |h| h.reserved() == 0);
                        server.shutdown(&mut ring).unwrap();
                        continue;
                    }
                    assert_eq!(
                        server.handler().errors.last(),
                        Some(&io::ErrorKind::WouldBlock)
                    );
                    let ready = exchange(&mut ring, &mut server, tcp, "/object", &finish).unwrap();
                    assert_eq!(
                        parse_fields(ready.headers().iter()).unwrap().kind,
                        Kind::Ready
                    );
                    tcp = ready.recycle().unwrap();
                }
                match case {
                    3 => {
                        server
                            .handler_mut()
                            .server
                            .poll(Instant::now() + MAX_TIMEOUT);
                    }
                    7 => server.handler_mut().abandon = true,
                    8 => server.handler_mut().server.clear(),
                    _ => (),
                }
                let target = if case == 2 { "/other" } else { "/object" };
                let frame = if case == 0 { &hello } else { &finish };
                rejected(exchange(&mut ring, &mut server, tcp, target, frame));
                assert_eq!(server.handler_mut().completed().is_some(), case == 4);
                assert!(server.handler_mut().completed().is_none());
            }
            until(&mut ring, &mut server, |h| h.reserved() == 0);
            assert!(server.handler_mut().completed().is_none());
            server.shutdown(&mut ring).unwrap();
        }
    }

    #[test]
    fn http_malicious_claims_and_cross_policy_proofs_never_establish() {
        let Some(mut ring) = control::tests::ring() else {
            return;
        };
        let b = context(3, 0, b"route");
        let listener = listener();
        let address = listener.local_addr().unwrap();
        let mut server = responder(listener, b.clone(), rails(&ring), false);
        for case in 0..10 {
            let a = context_with(2, 0, b"route", |s| match case {
                7 => s.volumes[0].topology.as_mut().unwrap().epoch += 1,
                8 => s.universe[0] ^= 1,
                9 => s.volumes[0].cache_generation += 1,
                _ => {}
            });
            let ao = offer(
                1,
                if case == 5 { [0; 16] } else { [5; 16] },
                u32::from(case == 6),
                2,
                if case == 4 { "foreign" } else { "fabric" },
            );
            let (initiator, hello) = start(&a, &b, "/arbitrary?x=1", &ao);
            let mut frame = a.frame(Kind::Hello, hello.encode().to_vec());
            if case < 4 {
                b.validate(&frame, a.prepared.local_node()).unwrap();
            }
            match case {
                0 => frame.node = NodeId::from_bytes(&[9; 32]).unwrap(),
                1 => frame.volume[0] ^= 1,
                2 => frame.shard = 1,
                3 => frame.routing[0] ^= 1,
                _ => {}
            }
            if case < 4 {
                rejected(b.validate(&frame, a.prepared.local_node()));
            }
            let result = exchange(
                &mut ring,
                &mut server,
                client::Connection::new(address, "localhost").unwrap(),
                "/arbitrary?x=1",
                &frame,
            );
            drop(initiator);
            // Topology context now rejects foreign universe/crypto generations even
            // before the authentication challenge can be answered.
            assert!(result.is_err(), "malicious case {case}");
            until(&mut ring, &mut server, |h| {
                assert!(h.server.store.borrow().completed.is_empty());
                h.reserved() == 0
            });
        }
        server.shutdown(&mut ring).unwrap();
    }
}
