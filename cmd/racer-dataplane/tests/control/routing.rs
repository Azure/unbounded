// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

mod tests {
    use super::*;

    fn routing(p: u32, local: &[u32], algorithm: Option<u32>) -> Routing {
        let degree = Topology::new(p, Epoch::new(1)).unwrap().degree();
        let slots: BTreeSet<_> = local
            .iter()
            .flat_map(|&u| {
                (0..degree).map(move |a| {
                    ((u64::from(u) * u64::from(degree) + u64::from(a)) % u64::from(p)) as u32
                })
            })
            .filter(|s| !local.contains(s))
            .collect();
        let volume = proto::Volume {
            id: "v".into(),
            origin_socket: "/tmp/origin.sock".into(),
            peers: slots.iter().map(|s| s.to_string()).collect(),
            topology: Some(proto::Topology {
                product: None,
                epoch: 1,
                slot_count: p,
                local_slots: local.to_vec(),
                neighbors: slots
                    .iter()
                    .map(|&slot| proto::SlotPeer {
                        slot,
                        peer: slot.to_string(),
                    })
                    .collect(),
                routing_algorithm: algorithm,
            }),
            ..Default::default()
        };
        Routing::new(&[0; 32], &volume).unwrap()
    }

    fn cursor(r: &Routing, source: u32, owner: u32, position: u8) -> Cursor {
        Cursor {
            path: Vec::new(),
            failed: u32::MAX,
            repair_position: 0,
            algorithm: r.algorithm,
            identity: r.identity,
            source,
            owner,
            attempt: 0,
            position,
        }
    }

    #[test]
    fn metadata_and_pages_have_independent_stable_owner_chains() {
        use crate::cache::{Namespace, PeerDescriptor, PeerPage};
        use crate::metadata::Checksum;
        let mut r = routing(257, &[0], Some(1));
        let namespace = Namespace::new("striping-test").unwrap();
        let size = crate::buffers::BUFFER_SIZE as u64;
        let metadata = PeerDescriptor::metadata("/large?exact=%2f")
            .key(namespace)
            .unwrap();
        let page = |offset, length, version, namespace| {
            PeerDescriptor::page(
                "/large?exact=%2f",
                PeerPage::new(offset, length, Checksum(version)),
            )
            .key(namespace)
            .unwrap()
        };
        let keys = (0..64)
            .map(|n| page(n * size, 64 * size, [1; 32], namespace))
            .collect::<Vec<_>>();
        let owners = keys
            .iter()
            .map(|key| r.start_key(key).owner)
            .collect::<BTreeSet<_>>();
        assert!(
            owners.len() > 40,
            "pages distribute across independent slots"
        );
        assert!(!keys.contains(&metadata));
        // A different placement must rebase each page using its own key,
        // preserving the bounded candidate attempt rather than metadata's owner.
        let mut next = routing(263, &[0], Some(1));
        next.identity[0] ^= 1;
        for key in std::iter::once(&metadata).chain(&keys) {
            let mut cursor = r.start_key(key);
            cursor.attempt = 2;
            let rebased = next.receive(cursor.clone(), key).unwrap();
            assert_eq!(rebased.owner, next.start_key(key).owner);
            assert_eq!(rebased.identity, next.identity);
            assert_eq!((rebased.attempt, rebased.position), (2, 0));
            next.validate(&rebased, key).unwrap();
            cursor.attempt = 8;
            assert!(next.receive(cursor, key).is_err());
        }
        assert_ne!(keys[0], page(0, 64 * size, [2; 32], namespace));
        assert_ne!(keys[0], page(0, 65 * size, [1; 32], namespace));
        assert_ne!(
            keys[0],
            page(0, 64 * size, [1; 32], Namespace::new("other").unwrap())
        );
        assert!(
            PeerDescriptor::page("/large", PeerPage::new(1, size, Checksum([1; 32])))
                .key(namespace)
                .is_err()
        );
        for key in std::iter::once(&metadata).chain(&keys) {
            let mut cursor = r.start_key(key);
            let expected = (u64::from_le_bytes(key[..8].try_into().unwrap()) % 257) as u32;
            assert_eq!(cursor.owner, expected);
            for attempt in 0..257 {
                cursor.attempt = attempt;
                assert_eq!(r.destination(&cursor), (expected + attempt) % 257);
                r.validate(&cursor, key).unwrap();
            }
            cursor.attempt = 257;
            assert!(r.validate(&cursor, key).is_err());
        }
        let before = keys
            .iter()
            .map(|key| r.start_key(key).owner)
            .collect::<Vec<_>>();
        r.geometry = Topology::new(257, Epoch::new(99)).unwrap();
        assert_eq!(
            before,
            keys.iter()
                .map(|key| r.start_key(key).owner)
                .collect::<Vec<_>>()
        );
        // Independent hashing allows collisions; it does not reserve a slot per page.
        let one = routing(1, &[0], Some(1));
        assert!(
            keys.iter()
                .all(|key| one.start_key(key).owner == one.start_key(&metadata).owner)
        );
    }

    #[test]
    fn canonical_effective_slots_prevent_colocated_cross_dependencies() {
        use crate::buffers::{NetworkDependency, NetworkFlightKey, NetworkProgress};
        let a = routing(5, &[0, 4], Some(1));
        let b = routing(5, &[2, 3], Some(1));
        // Two physical hosts have crossing dependencies, even though logical
        // ranks strictly decrease: A4 -> B3 -> owner, B2 -> A0 -> owner.
        let a4 = cursor(&a, 4, 1, 0);
        let b2 = cursor(&b, 2, 1, 0);
        let (_, b3) = a.next(&a4).unwrap().unwrap();
        let (_, a0) = b.next(&b2).unwrap().unwrap();
        assert_eq!(
            a.dependency(&a4).unwrap(),
            NetworkDependency::Canonical { slot: 4 }
        );
        assert_eq!(
            a.dependency(&a0).unwrap(),
            NetworkDependency::Canonical { slot: 0 }
        );
        assert_eq!(
            b.dependency(&b2).unwrap(),
            NetworkDependency::Canonical { slot: 2 }
        );
        assert_eq!(
            b.dependency(&b3).unwrap(),
            NetworkDependency::Canonical { slot: 3 }
        );
        assert!(!a.compatible(&a4, &a0));
        assert!(!b.compatible(&b2, &b3));
        let pool = crate::buffers::io_test_pool(8);
        for value in [[1; 32], [2; 32]] {
            // metadata and payload
            let key = |r: &Routing, c: &Cursor| NetworkFlightKey {
                value,
                routing: r.identity,
                version: c.algorithm.wire_version(),
                destination: 1,
                dependency: r.dependency(c).unwrap(),
            };
            let mut flights: Vec<_> = [(&a, &a4), (&b, &b2), (&b, &b3), (&a, &a0)]
                .into_iter()
                .map(|(r, c)| pool.network_flight(key(r, c)).unwrap())
                .collect();
            for flight in &mut flights {
                assert!(matches!(
                    flight.poll(std::task::Waker::noop()),
                    NetworkProgress::Produce
                ));
            }
            let local0 = cursor(&a, 0, 1, 0);
            assert!(a.compatible(&a0, &local0));
            let mut joined = pool.network_flight(key(&a, &local0)).unwrap();
            assert!(matches!(
                joined.poll(std::task::Waker::noop()),
                NetworkProgress::Pending
            ));
            let failure = std::sync::Arc::new(crate::cache::Error::Unavailable);
            flights[3].finish(Err(failure.clone()));
            let NetworkProgress::Ready(Err(shared)) = joined.poll(std::task::Waker::noop()) else {
                panic!()
            };
            assert!(std::sync::Arc::ptr_eq(&failure, &shared));
        }
        // A non-contiguous local shortcut (0 -> 1 -> 3 -> 7) normalizes to
        // slot 3, whose suffix is identical to direct ingress at slot 3.
        let r = routing(8, &[0, 3], Some(1));
        let c = cursor(&r, 0, 7, 0);
        assert_eq!(r.normalized_position(&c).unwrap(), 2);
        assert!(r.compatible(&c, &cursor(&r, 3, 7, 0)));
        assert_eq!(r.next(&c).unwrap().unwrap().0, "7");
        let mut invalid = c;
        invalid.position = 1; // remote receiving slot cannot normalize into local
        assert!(r.dependency(&invalid).is_err());
        assert!(r.next(&invalid).is_err());
    }

    #[test]
    fn canonical_wire_identity_and_dependency_context() {
        let new = routing(8, &[1, 2], Some(1));
        let explicit = routing(8, &[1, 2], Some(1));
        assert_eq!(new.identity, explicit.identity);
        // Both generations can have live producers for the same stored value,
        // destination and effective local slot in one NUMA registry.
        let pool = crate::buffers::io_test_pool(2);
        let mut flights: Vec<_> = [&new]
            .into_iter()
            .map(|r| {
                let c = cursor(r, 1, 3, 0);
                pool.network_flight(crate::buffers::NetworkFlightKey {
                    value: [1; 32],
                    routing: r.identity,
                    version: r.algorithm.wire_version(),
                    destination: 3,
                    dependency: r.dependency(&c).unwrap(),
                })
                .unwrap()
            })
            .collect();
        for f in &mut flights {
            assert!(matches!(
                f.poll(std::task::Waker::noop()),
                crate::buffers::NetworkProgress::Produce
            ));
        }
        let a = cursor(&new, 1, 3, 0);
        assert_eq!(new.path(&a).unwrap(), [1, 3]);
        assert_eq!(
            Cursor::decode(&a.encode()).unwrap().algorithm,
            Algorithm::Canonical
        );
    }
}
