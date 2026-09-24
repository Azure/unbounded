// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

use super::*;
use crate::handlers::Backend;
use crate::{cache::peer_wire, http_client as client, tls};
use prost::Message;

#[test]
#[ignore = "invoked by the control-plane compiler integration test with real snapshots"]
fn compiler_snapshots_http_and_rdma() {
    // Snapshot hex is larger than a fault descriptor; decode independently.
    let decode = |name| {
        let value = std::env::var(name).unwrap();
        let bytes: Vec<u8> = value
            .as_bytes()
            .chunks_exact(2)
            .map(|p| u8::from_str_radix(std::str::from_utf8(p).unwrap(), 16).unwrap())
            .collect();
        proto::Snapshot::decode(bytes.as_slice()).unwrap()
    };
    let old = decode("RACER_MEMBERSHIP_OLD");
    let new = decode("RACER_MEMBERSHIP_NEW");
    let prepare = |s: proto::Snapshot| {
        Arc::new(prepare_snapshot(
            &crate::control::Trust {
                universe: s.universe.as_slice().try_into().unwrap(),
                node: s.node.as_slice().try_into().unwrap(),
            },
            s,
        ))
    };
    let a = prepare(old.clone());
    let b = prepare(new.clone());
    let trust = crate::control::Trust {
        universe: new.universe.as_slice().try_into().unwrap(),
        node: new.node.as_slice().try_into().unwrap(),
    };
    for case in 0..5 {
        let mut invalid = new.clone();
        match case {
            0 => invalid.volumes[0].member_catalog = Some(99),
            1 => {
                let duplicate = invalid.member_catalogs[0].members[0].clone();
                invalid.member_catalogs[0].members.push(duplicate);
            }
            2 => invalid.member_catalogs[0].members[0]
                .node
                .pop()
                .map(|_| ())
                .unwrap(),
            3 => invalid.peers[0].pod_uid = "substituted-process".into(),
            _ => invalid.member_catalogs[0].members[0].pod_uid.clear(),
        }
        assert!(
            trust
                .prepare(proto::Configuration {
                    contents: Some(proto::configuration::Contents::Snapshot(invalid))
                })
                .is_err(),
            "catalog case {case}"
        );
    }
    let volume = &new.volumes[0].id;
    let aid = tls::PeerIdentity::new(
        &peer_wire::hex(&old.universe),
        &peer_wire::hex(&old.node),
        "pod-0",
    )
    .unwrap();
    let bid = tls::PeerIdentity::new(
        &peer_wire::hex(&new.universe),
        &peer_wire::hex(&new.node),
        "pod-2",
    )
    .unwrap();
    assert!(!b.peers().contains_key(&aid.node));
    assert!(!b.volumes()[0].peers().contains_key(&aid.node));
    assert!(b.authorize_member(volume, &aid).is_ok());
    assert!(b.rdma_member(volume, a.local_node()));
    let context = Rc::new(negotiation::Context::new(b.clone(), volume, 0, ROUTING).unwrap());
    let mut manager = Manager {
        context: context.clone(),
        rails: negotiation::Rails::new(vec![None], 1).unwrap(),
        outbound: vec![],
        inbound: BTreeMap::new(),
        live: vec![],
    };
    let mut hint = negotiation::RequestHint {
        node: a.local_node(),
        volume: context.volume(),
        shard: 0,
        is_finish: false,
    };
    assert!(
        manager.incoming(&hint).is_ok(),
        "runtime RDMA dispatch admits nonadjacent member"
    );
    hint.volume[0] ^= 1;
    assert!(manager.incoming(&hint).is_err());
    let mut removed = new.clone();
    // Volume-scoped exclusion is tested without altering topology: choose a
    // second catalog retaining endpoints but excluding the nonadjacent sender.
    let mut subset = removed.member_catalogs[0].clone();
    subset.members.retain(|m| m.node != old.node);
    removed.member_catalogs.push(subset);
    removed.volumes[0].member_catalog = Some(1);
    let removed = prepare(removed);
    assert!(removed.authorize_member(volume, &aid).is_err());
    assert!(removed.authorize_member(&new.volumes[1].id, &aid).is_ok());
    // Indexing is shared across all sixteen volume policies.
    let policy = b.authentication(volume).unwrap();
    for v in b.volumes() {
        assert!(Arc::ptr_eq(
            &policy.members,
            &b.authentication(&v.config().id).unwrap().members
        ));
    }
    let ar = a.volumes()[0].routing();
    let br = b.volumes()[0].routing();
    let (target, cursor) = (0..10000)
        .find_map(|n| {
            let target = format!("/membership-{n}");
            let (peer, cursor) = ar.next(&ar.start(&target)).ok()??;
            (peer == bid.node && br.start(&target).owner == 2).then_some((target, cursor))
        })
        .expect("old A routes to B and new B owns target");
    assert_ne!(cursor.identity, br.identity);
    let namespace = crate::cache::Namespace::volume(
        &old.universe,
        volume,
        old.volumes[0].cache_generation,
        a.volumes()[0].backend().namespace(),
    );
    let key = crate::cache::PeerDescriptor::metadata(&target)
        .key(namespace)
        .unwrap();
    let rebased = br.receive(cursor.clone(), &key).unwrap();
    assert!(br.next(&rebased).unwrap().is_none());
    let mut inner = cursor.algorithm.magic().to_vec();
    inner.extend(cursor.encode());
    inner.extend(b"RD01\0");
    inner.extend(target.as_bytes());
    let wire = peer_wire::with_chain(
        peer_wire::with_budget(inner, Duration::from_secs(5)).unwrap(),
        *namespace.digest(),
        7,
        127,
        ar.destination(&cursor),
    )
    .unwrap();

    let Some(mut ring) = crate::conformance::kernel_ring(8, uring::Config::default()) else {
        panic!("io_uring required for compiler membership integration")
    };
    let ca = tls::tests::Authority::new();
    let server_tls = ca.context(&bid, false);
    let origin = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
    origin.set_nonblocking(true).unwrap();
    let backend = Backend::new(&origin.local_addr().unwrap().to_string(), volume).unwrap();
    let origin_task = std::thread::spawn(move || {
        use std::io::{Read, Write};
        let end = Instant::now() + Duration::from_secs(5);
        let mut socket = loop {
            match origin.accept() {
                Ok((socket, _)) => break socket,
                Err(e) if e.kind() == io::ErrorKind::WouldBlock => {
                    assert!(Instant::now() < end);
                    std::thread::sleep(Duration::from_millis(1));
                }
                Err(e) => panic!("{e}"),
            }
        };
        socket
            .set_read_timeout(Some(Duration::from_secs(5)))
            .unwrap();
        let mut request = Vec::new();
        while !request.ends_with(b"\r\n\r\n") {
            let mut b = [0];
            socket.read_exact(&mut b).unwrap();
            request.push(b[0]);
        }
        assert!(
            String::from_utf8(request)
                .unwrap()
                .starts_with(&format!("HEAD {target} HTTP/1.1"))
        );
        write!(socket, "HTTP/1.1 200 OK\r\nContent-Length: 3\r\nETag: {}\r\nCache-Control: max-age=60\r\nConnection: close\r\n\r\n", crate::conformance::etag(b"abc")).unwrap();
    });
    let mut handler = Handler::shared(
        Rc::new(RefCell::new(crate::cache::tests::cache(1))),
        backend,
        namespace,
    );
    handler.set_authentication(policy);
    handler.set_routing(br.clone(), BTreeMap::new());
    let generation = Rc::new(Generation {
        volume: volume.clone(),
        handler: Rc::new(RefCell::new(handler)),
        _config: b.clone(),
        manager: None,
        active: Cell::new(true),
        drain: Cell::new(None),
        expired: Cell::new(false),
    });
    let mut listener = http::Listener::bind(
        "127.0.0.1:0".parse().unwrap(),
        std::num::NonZeroU32::new(32).unwrap(),
    )
    .unwrap();
    listener.set_tls(
        server_tls,
        tls::ExpectedPeer::Universe(bid.universe.clone()),
    );
    let address = listener.local_addr().unwrap();
    let mut server = http::Server::new(
        listener,
        PeerHandler {
            config: Some(b.clone()),
            volumes: BTreeMap::from([(
                volume.clone(),
                VolumeHandler {
                    local: false,
                    current: generation.clone(),
                    draining: vec![],
                },
            )]),
        },
        http::Config::default(),
    );
    // Successful origin metadata proves admission and local-placement rebasing.
    for (which, expected) in [
        (0, Some(200)),
        (1, None),
        (2, Some(400)),
        (3, None),
        (4, Some(400)),
    ] {
        let mut identity = aid.clone();
        if which == 1 {
            identity.pod_uid = "replaced-pod".into();
        }
        let mut bytes = wire.clone();
        if which == 2 {
            let foreign = crate::cache::Namespace::volume(
                &old.universe,
                volume,
                old.volumes[0].cache_generation + 1,
                a.volumes()[0].backend().namespace(),
            );
            bytes[4..36].copy_from_slice(foreign.digest());
        }
        if which == 4 {
            let foreign = crate::cache::Namespace::volume(
                &old.universe,
                "recreated-cache-uid",
                old.volumes[0].cache_generation,
                a.volumes()[0].backend().namespace(),
            );
            bytes[4..36].copy_from_slice(foreign.digest());
        }
        let context = ca.context(&identity, false);
        let hex = peer_wire::hex(&bytes);
        let selected_volume = if which == 3 { "foreign-cache" } else { volume };
        let end = Instant::now() + Duration::from_secs(5);
        let mut get = client::Connection::new_tls(
            address,
            "localhost",
            &context,
            tls::ExpectedPeer::Identity(bid.clone()),
        )
        .unwrap()
        .get_small(
            client::Request::new(
                "/",
                &[
                    ("X-Racer-Volume", selected_volume),
                    ("X-Racer-Fault", &hex),
                    (
                        "X-Racer-Attempt",
                        &format!(
                            "{}{}",
                            peer_wire::hex(
                                crate::authorization::binding(&bytes, &Default::default())
                                    .as_bytes()
                            ),
                            "a".repeat(32)
                        ),
                    ),
                ],
            )
            .unwrap(),
            crate::cache::METADATA_SIZE,
            end,
        )
        .unwrap();
        let actual = loop {
            assert!(Instant::now() < end);
            ring.progress().unwrap();
            server.poll(&mut ring, 64).unwrap();
            match get.poll(&mut ring, 64) {
                Ok(Progress::Ready(response)) => break Some(response.status()),
                Err(_) => break None,
                _ => {}
            }
        };
        assert_eq!(actual, expected, "HTTP membership case {which}");
    }
    origin_task.join().unwrap();
    crate::negotiation::tests::compiler_membership(&mut ring, a, b, &aid, &bid, &ca, ROUTING);
    server.shutdown(&mut ring).unwrap();
    generation.handler.borrow_mut().shutdown(&mut ring).unwrap();
    ring.shutdown().unwrap();
}
