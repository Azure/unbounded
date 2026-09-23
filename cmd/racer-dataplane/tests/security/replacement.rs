// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Native TLS/HTTP and production negotiation, with only verbs modeled. Keeping
// source polling separate lets the test stop after ACK write and before receipt.
struct ReplacementResponder {
    server: Server,
    starts: usize,
    errors: Vec<(io::ErrorKind, String)>,
}

impl http::Handler for ReplacementResponder {
    type Task = io::Result<Task>;

    fn start(&mut self, request: http::Request) -> Self::Task {
        self.starts += 1;
        let result = self.server.start(request);
        if let Err(error) = &result {
            self.errors.push((error.kind(), error.to_string()));
        }
        result
    }

    fn poll(
        &mut self,
        task: &mut Self::Task,
        ring: &mut Ring,
        budget: usize,
    ) -> io::Result<Progress<http::Completed>> {
        self.server.poll_task(
            task.as_mut().map_err(|e| io::Error::other(e.to_string()))?,
            ring,
            budget,
        )
    }
}

struct ReplacementFixture {
    ca: crate::tls::tests::Authority,
    aid: crate::tls::PeerIdentity,
    bid: crate::tls::PeerIdentity,
    address: std::net::SocketAddr,
    a: Rc<Context>,
    b: Rc<Context>,
    at: rdma::Transport,
    bt: rdma::Transport,
    asource: rdma::Source,
    bsource: rdma::Source,
    http: http::Server<ReplacementResponder>,
}

impl ReplacementFixture {
    fn new(ring: &Ring) -> Self {
        use crate::tls::{ExpectedPeer, tests::Authority};
        let ca = Authority::new();
        let b = context(3, 0, b"route");
        let aid = b
            .prepared
            .peer_identity(&NodeId::from_bytes(&[2; 32]).unwrap().to_string())
            .unwrap();
        let mut listener = http::Listener::bind(
            "127.0.0.1:0".parse().unwrap(),
            std::num::NonZeroU32::new(8).unwrap(),
        )
        .unwrap();
        let address = listener.local_addr().unwrap();
        let a = context_with(2, 0, b"route", |s| {
            s.peers[0].http_address = address.to_string()
        });
        let bid = a
            .prepared
            .peer_identity(&b.prepared.local_node().to_string())
            .unwrap();
        listener.set_tls(
            ca.context(&bid, false),
            ExpectedPeer::Universe(bid.universe.clone()),
        );
        let provider = Provider::for_test(aid.clone(), ca.context(&aid, false));
        let a = Rc::new((*a).clone().with_credentials(Some(provider)));
        let at = rdma::test_transport_config(ring.pool(), 1, 1);
        let bt = rdma::test_transport_config(ring.pool(), 1, 1);
        let server = Server::new(
            b.clone(),
            Rails::new(vec![Some(bt.clone())], 1).unwrap(),
            1,
            Duration::from_secs(5),
        )
        .unwrap();
        Self {
            ca,
            aid,
            bid,
            address,
            a,
            b,
            asource: at.test_source(),
            bsource: bt.test_source(),
            at,
            bt,
            http: http::Server::new(
                listener,
                ReplacementResponder {
                    server,
                    starts: 0,
                    errors: Vec::new(),
                },
                http::Config::default(),
            ),
        }
    }

    fn start(&self) -> Client {
        Client::start(
            self.a.clone(),
            Rails::new(vec![Some(self.at.clone())], 1).unwrap(),
            &self.b.prepared.local_node().to_string(),
            "/replace",
            Duration::from_secs(5),
        )
        .unwrap()
    }

    fn tick(&mut self, ring: &mut Ring, a: bool, b: bool) {
        ring.progress().unwrap();
        self.http.poll(ring, 16).unwrap();
        if a {
            crate::uring::CompletionSource::poll(&mut self.asource, ring, 16).unwrap();
        }
        if b {
            crate::uring::CompletionSource::poll(&mut self.bsource, ring, 16).unwrap();
        }
        assert!(self.http.handler().server.reserved() <= 1);
        assert!(self.at.test_observe().qps <= 1);
        assert!(self.bt.test_observe().qps <= 1);
    }

    fn ack_without_receipt(&mut self, ring: &mut Ring) -> (Client, Rc<rdma::Connection>) {
        let mut client = self.start();
        let end = Instant::now() + Duration::from_secs(5);
        loop {
            self.tick(ring, false, false);
            assert!(matches!(
                client.poll(ring, 16).unwrap(),
                Progress::Pending(_)
            ));
            if matches!(client.state, ClientState::Confirming(_))
                && !self
                    .http
                    .handler()
                    .server
                    .store
                    .borrow()
                    .completed
                    .is_empty()
            {
                break;
            }
            assert!(Instant::now() < end, "HTTP offer takeover stalled");
        }
        assert_eq!(self.http.handler().server.reserved(), 1);
        let old = Rc::new(
            self.http
                .handler_mut()
                .server
                .take_completed(Instant::now())
                .unwrap()
                .connection,
        );
        // Send Confirm, but do not poll the initiator source again. The ACK
        // travels on TLS, so modeled verbs confirmation pumping is insufficient.
        self.tick(ring, true, false);
        while !old.is_confirmed() {
            self.tick(ring, false, true);
            assert!(Instant::now() < end, "responder ACK write stalled");
        }
        let ClientState::Confirming(connection) = &client.state else {
            panic!("initiator must still own the uncollected session");
        };
        assert!(!connection.is_confirmed(), "ACK unexpectedly consumed");
        assert!(old.is_healthy() && old.authenticated_received());
        assert_eq!(self.http.handler().server.reserved(), 0);
        self.http.handler_mut().server.replacement = Some(old.clone());
        (client, old)
    }

    fn rejected_client(&mut self, ring: &mut Ring, poll_responder: bool) {
        let before = self.http.handler().starts;
        let mut client = self.start();
        let end = Instant::now() + Duration::from_secs(5);
        loop {
            self.tick(ring, true, poll_responder);
            match client.poll(ring, 16) {
                Err(error) => {
                    assert_ne!(error.kind(), io::ErrorKind::TimedOut);
                    break;
                }
                Ok(Progress::Pending(_)) => (),
                Ok(Progress::Ready(_)) => panic!("full-capacity offer unexpectedly admitted"),
            }
            assert!(Instant::now() < end, "replacement rejection stalled");
        }
        assert_eq!(self.http.handler().starts, before + 1);
        assert_eq!(
            self.http.handler().errors.last().unwrap().0,
            io::ErrorKind::WouldBlock
        );
        assert_eq!(self.http.handler().server.reserved(), 0);
        assert!(
            self.http
                .handler()
                .server
                .store
                .borrow()
                .completed
                .is_empty()
        );
        drop(client);
        assert_eq!(self.at.test_observe().qps, 0);
    }

    fn reject_offer(&mut self, ring: &mut Ring, tls: &crate::tls::TlsContext, frame: &Frame) {
        let tcp = client::Connection::new_tls(
            self.address,
            "localhost",
            tls,
            crate::tls::ExpectedPeer::Identity(self.bid.clone()),
        )
        .unwrap();
        let encoded = frame.encode();
        let end = Instant::now() + Duration::from_secs(5);
        let mut exchange = tcp
            .head(
                client::Request::new("/replace", &[(HEADER, &encoded)]).unwrap(),
                end,
            )
            .unwrap();
        loop {
            self.tick(ring, true, true);
            match exchange.poll(ring, 16) {
                Err(error) => {
                    assert_ne!(error.kind(), io::ErrorKind::TimedOut);
                    break;
                }
                Ok(Progress::Pending(_)) => (),
                Ok(Progress::Ready(_)) => panic!("malicious replacement accepted"),
            }
            assert!(Instant::now() < end, "malicious offer rejection stalled");
        }
        assert_eq!(self.http.handler().server.reserved(), 0);
        assert!(
            self.http
                .handler()
                .server
                .store
                .borrow()
                .completed
                .is_empty()
        );
    }

    fn shutdown(mut self, ring: &mut Ring) {
        self.http.shutdown(ring).unwrap();
        drop(self.http);
        self.at.shutdown().unwrap();
        self.bt.shutdown().unwrap();
        assert_eq!(self.at.test_invariants(), (0, 0, 0));
        assert_eq!(self.bt.test_invariants(), (0, 0, 0));
    }
}
