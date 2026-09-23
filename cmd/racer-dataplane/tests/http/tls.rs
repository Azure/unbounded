// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

mod tls_transport {
    use super::*;
    use crate::tls::{ExpectedPeer, PeerIdentity, tests::Authority};

    fn identity(node: char) -> PeerIdentity {
        PeerIdentity::new(&"a".repeat(64), &node.to_string().repeat(64), "pod-1").unwrap()
    }

    fn assert_offload(ktls: bool, before: crate::tls::TlsCounters) {
        let after = crate::tls::global_counters();
        assert_eq!(after.handshakes - before.handshakes, 2);
        if ktls && std::env::var_os("RACER_REQUIRE_KTLS").is_some() {
            // Exactly one production HTTP client and server handshook. Requiring
            // both increments catches attaching the client's BIO before connect.
            assert_eq!(after.ktls_tx_connections - before.ktls_tx_connections, 2);
            assert_eq!(after.ktls_rx_connections - before.ktls_rx_connections, 2);
        } else if !ktls {
            assert_eq!(after.ktls_tx_connections, before.ktls_tx_connections);
            assert_eq!(after.ktls_rx_connections, before.ktls_rx_connections);
        }
    }

    struct HeldEcho {
        echo: Echo,
        held: bool,
    }
    impl Handler for HeldEcho {
        type Task = Task;
        fn start(&mut self, request: Request) -> Task {
            assert_eq!(request.peer_identity(), Some(&identity('c')));
            self.echo.start(request)
        }
        fn poll(
            &mut self,
            task: &mut Task,
            ring: &mut Ring,
            budget: usize,
        ) -> io::Result<Progress<Completed>> {
            if self.held {
                return Ok(pending(true, None));
            }
            self.echo.poll(task, ring, budget)
        }
    }

    #[test]
    fn encrypted_http_kernel_integration() {
        cache_responses::kernel_child(
            "http_server::tests::tls_transport::encrypted_http_child",
            "RACER_HTTP_TLS_CHILD",
        );
    }

    #[test]
    #[ignore = "run via bounded encrypted_http_kernel_integration subprocess"]
    fn encrypted_http_child() {
        if std::env::var_os("RACER_HTTP_TLS_CHILD").is_none() {
            return;
        }
        let Some(mut ring) = crate::conformance::kernel_ring(
            4,
            crate::uring::Config {
                progress_reserve: 0,
                entries: 16,
                requests: 32,
                fixed_files: 8,
                completion_budget: 4,
                shutdown_timeout: Duration::from_millis(500),
            },
        ) else {
            return;
        };
        for ktls in [false, true] {
            duplex_channel(&mut ring, ktls);
            roundtrip_rotation_and_rejection(&mut ring, ktls);
            rotation_bounds_inbound_reuse(&mut ring, ktls);
            file_roundtrip(&mut ring, ktls, false);
            file_roundtrip(&mut ring, ktls, true);
        }
        ring.shutdown().unwrap();
    }

    fn rotation_bounds_inbound_reuse(ring: &mut Ring, ktls: bool) {
        let ca = Authority::new();
        let mut listener = listener();
        listener.set_tls(
            ca.context(&identity('b'), ktls),
            ExpectedPeer::Universe(identity('b').universe),
        );
        listener.set_tls_revision(10);
        let address = listener.local_addr().unwrap();
        let mut server = Server::new(
            listener,
            HeldEcho {
                echo: Echo { requests: 0 },
                held: true,
            },
            Config::default(),
        );
        let client = http_client::Connection::new_tls(
            address,
            "peer",
            &ca.context(&identity('c'), ktls),
            ExpectedPeer::Identity(identity('b')),
        )
        .unwrap();
        let mut request = client
            .head(
                http_client::Request::new("/object?version=1", &[]).unwrap(),
                deadline(),
            )
            .unwrap();
        drive(ring, |ring| {
            let mut work = server.poll(ring, 4)?;
            let Progress::Pending(pending) = request.poll(ring, 4)? else {
                panic!("held request completed")
            };
            work.merge(pending);
            Ok(if server.handler().echo.requests == 1 {
                Progress::Ready(())
            } else {
                Progress::Pending(work)
            })
        })
        .unwrap();
        let original = server
            .slots
            .front()
            .unwrap()
            .response_deadline
            .as_ref()
            .unwrap()
            .get();
        let before = crate::environment::now();
        server.install_tls(
            ca.context(&identity('b'), ktls),
            ExpectedPeer::Universe(identity('b').universe),
            11,
            u64::MAX,
        );
        assert!(
            server.slots.front().unwrap().tls_retire.unwrap() >= before + Duration::from_secs(30)
        );
        // Advance only the admission grace, leaving the actual certificate and
        // admitted request deadline valid. Repeated installation cannot renew it.
        server.slots.front_mut().unwrap().tls_retire = Some(before);
        server.install_tls(
            ca.context(&identity('b'), ktls),
            ExpectedPeer::Universe(identity('b').universe),
            12,
            u64::MAX,
        );
        assert_eq!(server.slots.front().unwrap().tls_retire, Some(before));
        assert_eq!(
            server
                .slots
                .front()
                .unwrap()
                .response_deadline
                .as_ref()
                .unwrap()
                .get(),
            original
        );
        assert!(http_client::TlsChannel::old_connections(12) > 0);
        server.handler_mut().held = false;
        let response = drive(ring, |ring| {
            let work = server.poll(ring, 4)?;
            Ok(match request.poll(ring, 4)? {
                Progress::Pending(mut pending) => {
                    pending.merge(work);
                    Progress::Pending(pending)
                }
                ready => ready,
            })
        })
        .unwrap();
        assert_eq!(response.status(), 200);
        until(ring, |ring| {
            server.poll(ring, 4).unwrap();
            server.connections() == 0
        });
        assert_eq!(http_client::TlsChannel::old_connections(12), 0);
        assert_eq!(server.handler().echo.requests, 1);
        drop(response);
        server.shutdown(ring).unwrap();
    }

    fn duplex_channel(ring: &mut Ring, ktls: bool) {
        let ca = Authority::new();
        let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        let socket = std::net::TcpStream::connect(listener.local_addr().unwrap()).unwrap();
        let (accepted, _) = listener.accept().unwrap();
        let mut client = http_client::TlsChannel::new(
            crate::uring::File::new(socket.into()),
            &ca.context(&identity('c'), ktls),
            ExpectedPeer::Identity(identity('b')),
            false,
        )
        .unwrap();
        let mut server = http_client::TlsChannel::new(
            crate::uring::File::new(accepted.into()),
            &ca.context(&identity('b'), ktls),
            ExpectedPeer::Identity(identity('c')),
            true,
        )
        .unwrap();
        let end = deadline();
        drive(ring, |ring| {
            let a = client.handshake(ring, end)?;
            let b = server.handshake(ring, end)?;
            let mut work = Work::default();
            if let Progress::Pending(w) = a {
                work.merge(w);
            }
            if let Progress::Pending(w) = b {
                work.merge(w);
            }
            Ok(
                if client.admits_new_request() && server.admits_new_request() {
                    Progress::Ready(())
                } else {
                    Progress::Pending(work)
                },
            )
        })
        .unwrap();
        // An idle read must not prevent writes on the same persistent channel.
        let mut input = [0; 16];
        assert!(matches!(
            client.poll_read(ring, &mut input, end).unwrap(),
            Progress::Pending(_)
        ));
        assert!(matches!(
            server.poll_read(ring, &mut input, end).unwrap(),
            Progress::Pending(_)
        ));
        // Rotation/expiry fence new operations, while admitted transfers finish.
        client.set_expiry(0);
        server.set_expiry(0);
        assert!(!client.admits_new_request());
        assert!(!server.admits_new_request());
        assert_eq!(
            drive(ring, |ring| client.poll_write(ring, b"client", end)).unwrap(),
            6
        );
        assert_eq!(
            drive(ring, |ring| server.poll_write(ring, b"server", end)).unwrap(),
            6
        );
        assert_eq!(
            drive(ring, |ring| client.poll_read(ring, &mut input, end)).unwrap(),
            6
        );
        assert_eq!(&input[..6], b"server");
        assert_eq!(
            drive(ring, |ring| server.poll_read(ring, &mut input, end)).unwrap(),
            6
        );
        assert_eq!(&input[..6], b"client");
    }

    fn roundtrip_rotation_and_rejection(ring: &mut Ring, ktls: bool) {
        let ca = Authority::new();
        let server_identity = identity('b');
        let client_identity = identity('c');
        let context = ca.context(&client_identity, ktls);
        let mut listener = listener();
        listener.set_tls(
            ca.context(&server_identity, ktls),
            ExpectedPeer::Universe(server_identity.universe.clone()),
        );
        let address = listener.local_addr().unwrap();
        let mut server = Server::new(
            listener,
            HeldEcho {
                echo: Echo { requests: 0 },
                held: false,
            },
            Config::default(),
        );
        let client = http_client::Connection::new_tls(
            address,
            "peer",
            &context,
            ExpectedPeer::Identity(server_identity.clone()),
        )
        .unwrap();
        let mut head = client
            .head(
                http_client::Request::new("/object?version=1", &[]).unwrap(),
                deadline(),
            )
            .unwrap();
        let before = crate::tls::global_counters();
        let response = drive(ring, |ring| {
            let work = server.poll(ring, 4)?;
            Ok(match head.poll(ring, 4)? {
                Progress::Pending(mut pending) => {
                    pending.merge(work);
                    Progress::Pending(pending)
                }
                ready => ready,
            })
        })
        .unwrap();
        assert_offload(ktls, before);
        assert_eq!(response.content_length(), Some(9999999));
        let client = response.recycle().unwrap();
        // Existing sessions retain their context across a listener replacement.
        server.install_tls(
            ca.context(&server_identity, ktls),
            ExpectedPeer::Universe(server_identity.universe.clone()),
            2,
            u64::MAX,
        );
        server.handler_mut().held = true;
        let fill = ring.pool().stage(Key::new([91; 32])).unwrap();
        let mut get = client
            .get(
                http_client::Request::new("/object?version=1", &[]).unwrap(),
                fill,
                deadline(),
            )
            .unwrap();
        drive(ring, |ring| {
            let mut work = server.poll(ring, 4)?;
            let Progress::Pending(pending) = get.poll(ring, 4)? else {
                panic!("held response completed")
            };
            work.merge(pending);
            Ok(if server.handler().echo.requests == 2 {
                Progress::Ready(())
            } else {
                Progress::Pending(work)
            })
        })
        .unwrap();
        // Rotate while the reused connection owns an admitted response. Its
        // deadline and old session survive, even if that session now expires.
        let slot = server.slots.front_mut().unwrap();
        let original_deadline = slot.deadline;
        let Some(Task::Headers(headers)) = &mut slot.task else {
            panic!("expected held GET headers")
        };
        let tls = headers
            .0
            .response
            .as_mut()
            .unwrap()
            .connection
            .tls
            .as_mut()
            .unwrap();
        assert_eq!(tls.revision(), 0);
        tls.set_expiry(0);
        assert!(!tls.admits_new_request());
        server.install_tls(
            ca.context(&server_identity, ktls),
            ExpectedPeer::Universe(server_identity.universe.clone()),
            3,
            u64::MAX,
        );
        assert_eq!(server.slots.front().unwrap().deadline, original_deadline);
        assert!(!server.slots.front().unwrap().control.closed.get());
        server.handler_mut().held = false;
        let mut response = drive(ring, |ring| {
            let work = server.poll(ring, 4)?;
            Ok(match get.poll(ring, 4)? {
                Progress::Pending(mut pending) => {
                    pending.merge(work);
                    Progress::Pending(pending)
                }
                ready => ready,
            })
        })
        .unwrap();
        assert_eq!(response.body(), [50; 3]);
        assert_eq!(server.handler().echo.requests, 2);
        let client = response.recycle().0.unwrap();
        let mut expired = client
            .head(
                http_client::Request::new("/object?version=1", &[]).unwrap(),
                deadline(),
            )
            .unwrap();
        assert!(
            drive(ring, |ring| {
                let work = server.poll(ring, 4)?;
                Ok(match expired.poll(ring, 4)? {
                    Progress::Pending(mut pending) => {
                        pending.merge(work);
                        Progress::Pending(pending)
                    }
                    ready => ready,
                })
            })
            .is_err()
        );
        assert_eq!(server.handler().echo.requests, 2);
        drop(expired);

        // The replacement listener accepts fresh sessions. The outbound context
        // and revision are captured before asynchronous connect begins.
        let mut client = http_client::Connection::new_tls(
            address,
            "peer",
            &ca.context(&client_identity, ktls),
            ExpectedPeer::Identity(server_identity.clone()),
        )
        .unwrap();
        client.set_tls_revision(7, u64::MAX);
        let mut head = client
            .head(
                http_client::Request::new("/object?version=1", &[]).unwrap(),
                deadline(),
            )
            .unwrap();
        let response = drive(ring, |ring| {
            let work = server.poll(ring, 4)?;
            Ok(match head.poll(ring, 4)? {
                Progress::Pending(mut pending) => {
                    pending.merge(work);
                    Progress::Pending(pending)
                }
                ready => ready,
            })
        })
        .unwrap();
        let mut channel = response.recycle().unwrap().into_tls_channel().unwrap();
        assert_eq!(channel.revision(), 7);
        assert_eq!(channel.peer_identity(), Some(&server_identity));
        assert!(channel.admits_new_request());
        channel.set_expiry(0);
        assert!(!channel.admits_new_request());
        drop(channel);
        assert_eq!(server.handler().echo.requests, 3);

        // Expiry captured before connect must reach the deferred native session.
        let mut client = http_client::Connection::new_tls(
            address,
            "peer",
            &context,
            ExpectedPeer::Identity(server_identity.clone()),
        )
        .unwrap();
        client.set_tls_revision(8, 0);
        let mut head = client
            .head(
                http_client::Request::new("/object?version=1", &[]).unwrap(),
                deadline(),
            )
            .unwrap();
        let error = drive(ring, |ring| {
            let work = server.poll(ring, 4)?;
            Ok(match head.poll(ring, 4)? {
                Progress::Pending(mut pending) => {
                    pending.merge(work);
                    Progress::Pending(pending)
                }
                ready => ready,
            })
        })
        .err()
        .expect("expired captured credentials must reject before HTTP");
        assert_eq!(error.kind(), io::ErrorKind::ConnectionAborted);
        assert_eq!(server.handler().echo.requests, 3);
        drop(head);

        // Matching node name with a different pod UID is not the same peer.
        let mut wrong = server_identity.clone();
        wrong.pod_uid = "replacement-pod".into();
        let client = http_client::Connection::new_tls(
            address,
            "peer",
            &context,
            ExpectedPeer::Identity(wrong),
        )
        .unwrap();
        let mut head = client
            .head(
                http_client::Request::new("/object?version=1", &[]).unwrap(),
                deadline(),
            )
            .unwrap();
        assert!(
            drive(ring, |ring| {
                let work = server.poll(ring, 4)?;
                Ok(match head.poll(ring, 4)? {
                    Progress::Pending(mut pending) => {
                        pending.merge(work);
                        Progress::Pending(pending)
                    }
                    ready => ready,
                })
            })
            .is_err()
        );
        drop(head);
        // A legacy plaintext request never reaches the HTTP handler.
        let client = http_client::Connection::new(address, "peer").unwrap();
        let mut head = client
            .head(
                http_client::Request::new("/object?version=1", &[]).unwrap(),
                deadline(),
            )
            .unwrap();
        assert!(
            drive(ring, |ring| {
                let work = server.poll(ring, 4)?;
                Ok(match head.poll(ring, 4)? {
                    Progress::Pending(mut pending) => {
                        pending.merge(work);
                        Progress::Pending(pending)
                    }
                    ready => ready,
                })
            })
            .is_err()
        );
        assert_eq!(server.handler().echo.requests, 3);
        drop(head);
        server.shutdown(ring).unwrap();
    }

    struct FileEcho(crate::allocator::FileValue);
    impl Handler for FileEcho {
        type Task = Task;
        fn start(&mut self, request: Request) -> Task {
            assert_eq!(request.peer_identity(), Some(&identity('c')));
            let Request::Get(request) = request else {
                panic!("expected GET")
            };
            Task::Headers(
                request
                    .respond(ResponseHead::new(200, Some((192 * 1024 + 7) as u64), &[]).unwrap())
                    .unwrap(),
            )
        }
        fn poll(
            &mut self,
            task: &mut Task,
            ring: &mut Ring,
            budget: usize,
        ) -> io::Result<Progress<Completed>> {
            let progress = match task {
                Task::Headers(headers) => headers.poll(ring, budget)?,
                Task::Body(body) => body.poll(ring, budget)?,
                _ => panic!("unexpected task"),
            };
            Ok(match progress {
                Progress::Pending(work) => Progress::Pending(work),
                Progress::Ready(BodyProgress::More(writer)) => {
                    *task = Task::Body(send_chunk(
                        writer,
                        BodyChunk::value(
                            crate::cache::CachedValue::File(self.0.clone()),
                            13..192 * 1024 + 20,
                        )
                        .unwrap(),
                    ));
                    pending(true, None)
                }
                Progress::Ready(BodyProgress::Done(done)) => {
                    *task = Task::Done;
                    Progress::Ready(done)
                }
            })
        }
    }

    fn file_roundtrip(ring: &mut Ring, ktls: bool, limited: bool) {
        use crate::allocator::{Allocator, Slab};
        let path =
            std::env::temp_dir().join(format!("racer-http-tls-{}-{ktls}.slab", std::process::id()));
        let io = if limited {
            crate::slab_io::Io::testing(50, 1, false)
        } else {
            crate::slab_io::Io::default()
        };
        let mut slab = io
            .scope(|| Slab::create(&path, 64 * 1024 * 1024, 1))
            .unwrap();
        let mut allocator = Allocator::open_inner(
            slab.take_shard(crate::workers::ShardId::at(0)).unwrap(),
            Default::default(),
        )
        .unwrap();
        allocator
            .insert_payload([92; 32], buffer(ring, 92, BUFFER_SIZE), None)
            .unwrap();
        let lease = allocator.lookup(&[92; 32], 0).unwrap();
        let value = drive(ring, |ring| {
            let work = allocator.poll(ring, 16)?;
            Ok(lease
                .ready()
                .map_or(Progress::Pending(work), Progress::Ready))
        })
        .unwrap();
        drive(ring, |ring| {
            let work = allocator.poll(ring, 16)?;
            Ok(if allocator.is_idle() {
                Progress::Ready(())
            } else {
                Progress::Pending(work)
            })
        })
        .unwrap();
        let counter = |name: &str| -> u64 {
            let mut text = String::new();
            io.render(&mut text);
            text.lines()
                .find_map(|line| {
                    line.strip_prefix(&format!("racer_dataplane_slab_io_{name} "))?
                        .parse()
                        .ok()
                })
                .unwrap()
        };
        let bytes_before = counter("bytes_total");
        let ops_before = counter("operations_total");
        let ca = Authority::new();
        let mut listener = listener();
        listener.set_tls(
            ca.context(&identity('b'), ktls),
            ExpectedPeer::Universe(identity('b').universe),
        );
        let address = listener.local_addr().unwrap();
        let mut server = Server::new(listener, FileEcho(value), Config::default());
        let client = http_client::Connection::new_tls(
            address,
            "peer",
            &ca.context(&identity('c'), ktls),
            ExpectedPeer::Identity(identity('b')),
        )
        .unwrap();
        let mut get = client
            .get(
                http_client::Request::new("/file", &[]).unwrap(),
                ring.pool().stage(Key::new([93; 32])).unwrap(),
                deadline(),
            )
            .unwrap();
        let before = crate::tls::global_counters();
        let mut response = drive(ring, |ring| {
            let mut work = allocator.poll(ring, 4)?;
            work.merge(server.poll(ring, 4)?);
            Ok(match get.poll(ring, 4)? {
                Progress::Pending(mut pending) => {
                    pending.merge(work);
                    Progress::Pending(pending)
                }
                ready => ready,
            })
        })
        .unwrap();
        assert_eq!(response.body(), vec![92; 192 * 1024 + 7]);
        if limited {
            assert_eq!(counter("bytes_total") - bytes_before, 192 * 1024 + 7);
            assert!(counter("operations_total") - ops_before >= 4);
            assert!(counter("waits_total") > 0);
        }
        assert_offload(ktls, before);
        let after = crate::tls::global_counters();
        if ktls && std::env::var_os("RACER_REQUIRE_KTLS").is_some() {
            assert!(after.ktls_tx_connections > before.ktls_tx_connections);
        }
        if ktls && after.ktls_tx_connections > before.ktls_tx_connections {
            assert_eq!(after.sendfile_bytes - before.sendfile_bytes, 192 * 1024 + 7);
        } else {
            assert_eq!(after.sendfile_bytes, before.sendfile_bytes);
            assert_eq!(
                after.fallback_sendfile_bytes - before.fallback_sendfile_bytes,
                192 * 1024 + 7
            );
        }
        drop(response);
        server.shutdown(ring).unwrap();
        drive(ring, |ring| {
            let work = allocator.poll(ring, 16)?;
            Ok(if allocator.is_idle() {
                Progress::Ready(())
            } else {
                Progress::Pending(work)
            })
        })
        .unwrap();
        drop((server, lease, allocator, slab));
        std::fs::remove_file(path).unwrap();
    }
}
