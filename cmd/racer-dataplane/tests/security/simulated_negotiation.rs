// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

use super::*;

struct Held {
    server: Server,
    hold: bool,
    started: bool,
    rejected: bool,
}
impl http::Handler for Held {
    type Task = io::Result<Task>;
    fn start(&mut self, request: http::Request) -> Self::Task {
        self.started = true;
        self.server.start(request)
    }
    fn poll(
        &mut self,
        task: &mut Self::Task,
        ring: &mut Ring,
        budget: usize,
    ) -> io::Result<Progress<http::Completed>> {
        if self.hold {
            return Ok(Progress::Pending(Work {
                runnable: true,
                deadline: None,
            }));
        }
        let result = self.server.poll_task(
            task.as_mut().map_err(|e| io::Error::other(e.to_string()))?,
            ring,
            budget,
        );
        self.rejected |= result.is_err();
        result
    }
}

#[test]
fn delayed_simulated_offer_rotation_retains_both_connection_revisions() {
    for rotate_client in [false, true] {
        let run = |replay| {
            let world = crate::simulation::World::new(814);
            let _scope = world.enter();
            world.enable_scheduler();
            if let Some(choices) = replay {
                world.replay(choices);
            }
            let mut ring =
                Ring::http_test_ring(crate::buffers::io_test_pool(4), Default::default()).unwrap();
            let address: std::net::SocketAddr = "127.0.0.1:9443".parse().unwrap();
            let (mut trust, snapshot) = crate::control::tests::fixture();
            let mut contexts = Vec::new();
            let mut providers = Vec::new();
            let ca = crate::tls::tests::Authority::new();
            for (node, remote) in [(2, 3), (3, 2)] {
                trust.node = [node; 32];
                let mut config = snapshot.clone();
                config.node = trust.node.to_vec();
                config.fabric = "fabric".into();
                config.peers[0].id = NodeId::from_bytes(&[remote; 32]).unwrap().to_string();
                config.peers[0].fabric = "fabric".into();
                config.peers[0].http_address = address.to_string();
                config.volumes[0].peers = vec![config.peers[0].id.clone()];
                config.volumes[0].topology = Some(crate::control::proto::Topology {
                    epoch: 1,
                    slot_count: 2,
                    local_slots: vec![(node - 2) as u32],
                    neighbors: vec![crate::control::proto::SlotPeer {
                        slot: (remote - 2) as u32,
                        peer: config.peers[0].id.clone(),
                    }],
                    ..Default::default()
                });
                crate::control::tests::scope_peers(&mut config);
                let prepared = crate::control::tests::prepare_snapshot(&trust, config);
                let identity = crate::tls::PeerIdentity::new(
                    &crate::cache::peer_wire::hex(&trust.universe),
                    &crate::cache::peer_wire::hex(&trust.node),
                    "test-pod",
                )
                .unwrap();
                let provider = Provider::for_test(identity.clone(), ca.context(&identity, false));
                contexts.push(Rc::new(
                    Context::new(Arc::new(prepared), "v1", 0, b"rotation")
                        .unwrap()
                        .with_credentials(Some(provider.clone())),
                ));
                providers.push(provider);
            }
            let at = rdma::test_transport_config(ring.pool(), 1, 1);
            let bt = rdma::test_transport_config(ring.pool(), 1, 1);
            let server = Server::new(
                contexts[1].clone(),
                Rails::new(vec![Some(bt.clone())], 1).unwrap(),
                1,
                Duration::from_secs(5),
            )
            .unwrap();
            let listener =
                http::Listener::bind(address, std::num::NonZeroU32::new(8).unwrap()).unwrap();
            let mut http = http::Server::new(
                listener,
                Held {
                    server,
                    hold: true,
                    started: false,
                    rejected: false,
                },
                Default::default(),
            );
            let install = |http: &mut http::Server<Held>| {
                let snapshot = providers[1].current();
                http.install_tls(
                    (*snapshot.context).clone(),
                    crate::tls::ExpectedPeer::Universe(providers[1].identity().universe.clone()),
                    snapshot.revision,
                    snapshot.expires_unix,
                );
                http.install_simulated_tls(providers[1].identity().clone());
            };
            install(&mut http);
            let mut client = Client::start(
                contexts[0].clone(),
                Rails::new(vec![Some(at.clone())], 1).unwrap(),
                &contexts[1].prepared.local_node().to_string(),
                "/rotation",
                Duration::from_secs(5),
            )
            .unwrap();
            let end = world.now() + Duration::from_secs(2);
            while !http.handler().started {
                world.advance(Duration::from_millis(1));
                ring.progress().unwrap();
                http.poll(&mut ring, 32).unwrap();
                assert!(matches!(
                    client.poll(&mut ring, 32).unwrap(),
                    Progress::Pending(_)
                ));
                assert!(world.now() < end);
            }
            let provider = &providers[if rotate_client { 0 } else { 1 }];
            provider.rotate_for_test(ca.context(provider.identity(), false));
            install(&mut http);
            http.handler_mut().hold = false;
            loop {
                world.advance(Duration::from_millis(1));
                ring.progress().unwrap();
                http.poll(&mut ring, 32).unwrap();
                if !rotate_client && http.handler().rejected {
                    assert!(
                        http.handler_mut()
                            .server
                            .take_completed(world.now())
                            .is_none()
                    );
                    break;
                }
                match client.poll(&mut ring, 32) {
                    Err(error) => {
                        assert_ne!(error.kind(), io::ErrorKind::TimedOut);
                        if rotate_client {
                            assert_eq!(error.kind(), io::ErrorKind::PermissionDenied);
                        } else {
                            assert!(http.handler().rejected);
                        }
                        break;
                    }
                    Ok(Progress::Ready(_)) => panic!("stale credential offer admitted"),
                    Ok(Progress::Pending(_)) => assert!(world.now() < end),
                }
            }
            drop(client);
            http.shutdown(&mut ring).unwrap();
            drop(http);
            at.shutdown().unwrap();
            bt.shutdown().unwrap();
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
