// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

mod tls_transport {
    use super::*;
    use crate::tls::{ExpectedPeer, PeerIdentity, tests::Authority};

    fn identity(node: char) -> PeerIdentity {
        PeerIdentity::new(&"a".repeat(64), &node.to_string().repeat(64), "pod-1").unwrap()
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
            file_roundtrip(&mut ring, ktls);
        }
        ring.shutdown().unwrap();
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
        let mut server = Server::new(listener, Echo { requests: 0 }, Config::default());
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
        assert_eq!(response.content_length(), Some(9999999));
        let client = response.recycle().unwrap();
        // Existing sessions retain their context across a listener replacement.
        server.install_tls(
            ca.context(&server_identity, ktls),
            ExpectedPeer::Universe(server_identity.universe.clone()),
            2,
            u64::MAX,
        );
        let fill = ring.pool().stage(Key::new([91; 32])).unwrap();
        let mut get = client
            .get(
                http_client::Request::new("/object?version=1", &[]).unwrap(),
                fill,
                deadline(),
            )
            .unwrap();
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
        assert_eq!(server.handler().requests, 2);
        drop(response);

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
        assert_eq!(server.handler().requests, 2);
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

    fn file_roundtrip(ring: &mut Ring, ktls: bool) {
        use crate::allocator::{Allocator, Slab};
        let path =
            std::env::temp_dir().join(format!("racer-http-tls-{}-{ktls}.slab", std::process::id()));
        let mut slab = Slab::create(&path, 64 * 1024 * 1024, 1).unwrap();
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
