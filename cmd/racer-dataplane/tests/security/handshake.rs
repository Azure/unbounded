// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

#[cfg(test)]
mod tests {
    use super::*;

    fn snapshot() -> Snapshot {
        crate::crypto::tests::trust(1).1
    }
    fn peers() -> PeerContext {
        PeerContext::new([1; 32], [2; 32]).unwrap()
    }
    fn start(snapshot: &Snapshot, offer: Option<&crate::rdma::Offer>) -> (Initiator, Hello) {
        Initiator::start(snapshot.clone(), peers(), offer, Duration::from_secs(60)).unwrap()
    }
    fn accept(
        snapshot: &Snapshot,
        hello: Hello,
        offer: Option<&crate::rdma::Offer>,
    ) -> (Responder, Reply) {
        Responder::accept(
            snapshot.clone(),
            peers(),
            hello,
            offer,
            Duration::from_secs(60),
        )
        .unwrap()
    }
    fn sessions(snapshot: &Snapshot) -> (Session, Session) {
        let (initiator, hello) = start(snapshot, None);
        let (responder, reply) = accept(snapshot, hello, None);
        let (a, finish) = initiator.finish(reply).unwrap();
        (a, responder.finish(finish).unwrap())
    }
    fn offer() -> crate::rdma::Offer {
        let mut bytes = vec![0; 77];
        bytes[..4].copy_from_slice(&2u32.to_be_bytes());
        bytes[4..20].fill(1); // nonce
        bytes[20..36].fill(2); // challenge
        bytes[36..52].fill(3); // GID
        bytes[52..56].copy_from_slice(&1u32.to_be_bytes());
        bytes[62] = 1; // MTU
        bytes[64] = 1; // reads
        bytes[70..74].copy_from_slice(&1u32.to_be_bytes());
        bytes[74..76].copy_from_slice(&1u16.to_be_bytes());
        bytes[76] = b'f';
        crate::rdma::Offer::decode(&bytes).unwrap()
    }

    #[test]
    fn handshake_binds_every_message_byte_including_nonempty_rdma_offers() {
        let snapshot = snapshot();
        let offer = offer();
        let (_, hello) = start(&snapshot, Some(&offer));
        for i in 0..hello.0.len() {
            let (initiator, mut hello) = start(&snapshot, Some(&offer));
            hello.0[i] ^= 1;
            match Responder::accept(
                snapshot.clone(),
                peers(),
                hello,
                Some(&offer),
                Duration::from_secs(60),
            ) {
                Err(Error::UnknownKey) if i < 32 => {}
                Ok((_, reply)) => assert!(matches!(
                    initiator.finish(reply),
                    Err(Error::Authentication)
                )),
                _ => panic!("unexpected hello rejection at {i}"),
            }
        }
        for i in 0..128 + offer.encode().len() {
            let (initiator, hello) = start(&snapshot, Some(&offer));
            let (_, mut reply) = accept(&snapshot, hello, Some(&offer));
            reply.0[i] ^= 1;
            assert!(
                matches!(initiator.finish(reply), Err(Error::Authentication)),
                "reply byte {i}"
            );
        }
        for i in 0..96 {
            let (initiator, hello) = start(&snapshot, Some(&offer));
            let (responder, reply) = accept(&snapshot, hello, Some(&offer));
            let (_, mut finish) = initiator.finish(reply).unwrap();
            finish.0[i] ^= 1;
            assert!(matches!(
                responder.finish(finish),
                Err(Error::Authentication)
            ));
        }
        let (initiator, hello) = start(&snapshot, Some(&offer));
        let (responder, reply) = accept(&snapshot, hello, Some(&offer));
        let (mut a, finish) = initiator.finish(reply).unwrap();
        let mut b = responder.finish(finish).unwrap();
        for session in [&mut a, &mut b] {
            assert_eq!(session.remote.as_deref(), Some(offer.encode().as_slice()));
            assert!(session.take_offer(&snapshot).unwrap().is_some());
            assert!(matches!(session.take_offer(&snapshot), Err(Error::Invalid)));
        }
        let (mut a, _) = sessions(&snapshot);
        assert!(a.take_offer(&snapshot).unwrap().is_none());
        assert!(matches!(a.take_offer(&snapshot), Err(Error::Invalid)));
    }

    #[test]
    fn handshake_replays_expiry_and_context_mismatch() {
        let snapshot = snapshot();
        let (initiator, hello) = start(&snapshot, None);
        let replayed_hello = Hello::decode(hello.encode()).unwrap();
        let (responder, reply) = accept(&snapshot, hello, None);
        let replayed_reply = Reply::decode(reply.encode()).unwrap();
        let (_, finish) = initiator.finish(reply).unwrap();
        let replayed_finish = Finish::decode(finish.encode()).unwrap();
        responder.finish(finish).unwrap();
        // Replaying Hello may elicit a challenge, but cannot reuse the old proof.
        let (responder, _) = accept(&snapshot, replayed_hello, None);
        assert!(matches!(
            responder.finish(replayed_finish),
            Err(Error::Authentication)
        ));
        let (initiator, _) = start(&snapshot, None);
        assert!(matches!(
            initiator.finish(replayed_reply),
            Err(Error::Authentication)
        ));
        let (mut initiator, hello) = start(&snapshot, None);
        let (_, reply) = accept(&snapshot, hello, None);
        initiator.deadline = Instant::now();
        assert!(matches!(initiator.finish(reply), Err(Error::Expired)));
        let (initiator, hello) = start(&snapshot, None);
        let (mut responder, reply) = accept(&snapshot, hello, None);
        let (_, finish) = initiator.finish(reply).unwrap();
        responder.deadline = Instant::now();
        assert!(matches!(responder.finish(finish), Err(Error::Expired)));
        for timeout in [Duration::ZERO, Duration::from_secs(61)] {
            assert!(matches!(
                Initiator::start(snapshot.clone(), peers(), None, timeout),
                Err(Error::Invalid)
            ));
            let (_, hello) = start(&snapshot, None);
            assert!(matches!(
                Responder::accept(snapshot.clone(), peers(), hello, None, timeout),
                Err(Error::Invalid)
            ));
        }
        for change in 0..2 {
            let other = Snapshot::signed(
                if change == 0 {
                    UniverseId::new([9; 32])
                } else {
                    snapshot.universe()
                },
                snapshot.signatures().clone(),
            );
            let expected = if change == 1 {
                PeerContext::new([2; 32], [1; 32]).unwrap()
            } else {
                peers()
            };
            let (initiator, hello) = start(&snapshot, None);
            let (_, reply) =
                Responder::accept(other, expected, hello, None, Duration::from_secs(60)).unwrap();
            assert!(matches!(
                initiator.finish(reply),
                Err(Error::Authentication)
            ));
        }
        assert!(matches!(
            PeerContext::new([1; 32], [1; 32]),
            Err(Error::Invalid)
        ));
    }

    #[test]
    fn handshake_and_control_decoder_boundaries() {
        for len in [0, 31, 32, 63, 64, 65, 64 + MAX_OFFER, 65 + MAX_OFFER] {
            let bytes = vec![0; len];
            let valid = (64..=64 + MAX_OFFER).contains(&len);
            assert_eq!(Hello::decode(&bytes).is_ok(), valid);
            if valid {
                assert_eq!(Hello::decode(&bytes).unwrap().encode(), bytes);
            }
        }
        for len in [0, 127, 128, 129, 128 + MAX_OFFER, 129 + MAX_OFFER] {
            assert_eq!(
                Reply::decode(&vec![0; len]).is_ok(),
                (128..=128 + MAX_OFFER).contains(&len)
            );
        }
        for len in [0, 95, 96, 97] {
            assert_eq!(Finish::decode(&vec![0; len]).is_ok(), len == 96);
        }
        for len in [0, 111, 112, 113, 112 + MAX_CONTROL, 113 + MAX_CONTROL] {
            assert_eq!(
                SignedControl::decode(&vec![0; len]).is_ok(),
                (112..=112 + MAX_CONTROL).contains(&len)
            );
        }
        for len in [0, 1, MAX_CONTROL, MAX_CONTROL + 1] {
            assert_eq!(Control::new(0, vec![0; len]).is_ok(), len <= MAX_CONTROL);
        }
        // Exercise the maximum transcript size, including both length-delimited offers.
        let s = snapshot();
        assert!(
            transcript(
                &peers()
                    .with_negotiation([3; 32], u64::MAX, &vec![0; MAX_ROUTING_CONTEXT])
                    .unwrap()
                    .bytes(&s),
                &vec![0; 64 + MAX_OFFER],
                &vec![0; 32 + MAX_OFFER]
            )
            .len()
                <= MAX_HANDSHAKE
        );
    }

    #[test]
    fn negotiation_context_is_bounded_domain_separated_and_fully_bound() {
        let snapshot = snapshot();
        let timeout = Duration::from_secs(60);
        let context = peers()
            .with_negotiation([3; 32], 17, b"routing-generation-9")
            .unwrap();
        let bytes = context.bytes(&snapshot);
        // Existing identity-only transcript remains byte-for-byte compatible.
        assert_eq!(peers().bytes(&snapshot).len(), 96);
        assert!(
            peers()
                .with_negotiation([0; 32], 0, &[])
                .unwrap()
                .bytes(&snapshot)
                .len()
                > 96
        );
        assert!(matches!(
            peers().with_negotiation([3; 32], 17, &vec![0; MAX_ROUTING_CONTEXT + 1]),
            Err(Error::Invalid)
        ));
        let maximum = peers()
            .with_negotiation([3; 32], 17, &vec![0; MAX_ROUTING_CONTEXT])
            .unwrap();
        let offer = offer();
        for good in [context.clone(), maximum] {
            let (a, hello) =
                Initiator::start(snapshot.clone(), good.clone(), Some(&offer), timeout).unwrap();
            let (b, reply) =
                Responder::accept(snapshot.clone(), good, hello, Some(&offer), timeout).unwrap();
            let (mut a, finish) = a.finish(reply).unwrap();
            let mut b = b.finish(finish).unwrap();
            assert!(a.take_offer(&snapshot).unwrap().is_some());
            assert!(b.take_offer(&snapshot).unwrap().is_some());
        }
        // Changing any volume, shard or routing byte invalidates the transcript.
        let offset = 96 + CONTEXT_DOMAIN.len();
        for index in offset..bytes.len() {
            let mut changed = context.clone();
            let binding = changed.negotiation.as_mut().unwrap();
            let index = index - offset;
            match index {
                0..32 => binding.volume[index] ^= 1,
                32..40 => binding.shard ^= 1 << ((39 - index) * 8),
                40..44 => continue, // Length is canonical, derived from bounded bytes.
                _ => binding.routing[index - 44] ^= 1,
            }
            let (a, hello) =
                Initiator::start(snapshot.clone(), context.clone(), Some(&offer), timeout).unwrap();
            let (_, reply) =
                Responder::accept(snapshot.clone(), changed, hello, Some(&offer), timeout).unwrap();
            assert!(matches!(a.finish(reply), Err(Error::Authentication)));
        }
        for changed in [
            peers(),
            peers()
                .with_negotiation([3; 32], 17, b"routing-generation-9\0")
                .unwrap(),
        ] {
            let (a, hello) =
                Initiator::start(snapshot.clone(), context.clone(), None, timeout).unwrap();
            let (_, reply) =
                Responder::accept(snapshot.clone(), changed, hello, None, timeout).unwrap();
            assert!(matches!(a.finish(reply), Err(Error::Authentication)));
        }
    }

    #[test]
    fn sequence_failures_preserve_state_and_both_directions_work() {
        let snapshot = snapshot();
        let (mut a, mut b) = sessions(&snapshot);
        let first = a
            .sign(&snapshot, Control::new(1, vec![42; MAX_CONTROL]).unwrap())
            .unwrap();
        let second = a.sign(&snapshot, Control::new(2, vec![]).unwrap()).unwrap();
        let later = second.encode().to_vec();
        assert!(matches!(b.verify(&snapshot, second), Err(Error::Sequence)));
        assert_eq!(b.rx, 0);
        for i in [0, 8, 16, first.0.len() - 1] {
            let mut bad = first.0.clone();
            bad[i] ^= 1;
            assert!(matches!(
                b.verify(&snapshot, SignedControl::decode(&bad).unwrap()),
                Err(Error::Authentication)
            ));
            assert_eq!(b.rx, 0);
        }
        assert_eq!(
            b.verify(&snapshot, first).unwrap().body(),
            vec![42; MAX_CONTROL]
        );
        assert_eq!(
            b.verify(&snapshot, SignedControl::decode(&later).unwrap())
                .unwrap()
                .request_id(),
            2
        );
        assert!(matches!(
            b.verify(&snapshot, SignedControl::decode(&later).unwrap()),
            Err(Error::Sequence)
        ));
        assert_eq!(b.rx, 2);
        let reverse = b
            .sign(&snapshot, Control::new(3, vec![7]).unwrap())
            .unwrap();
        assert_eq!(a.verify(&snapshot, reverse).unwrap().body(), &[7]);
        a.tx = u64::MAX - 1;
        b.rx = u64::MAX - 1;
        let last = a.sign(&snapshot, Control::new(4, vec![]).unwrap()).unwrap();
        let replay = last.encode().to_vec();
        b.verify(&snapshot, last).unwrap();
        assert!(matches!(
            a.sign(&snapshot, Control::new(5, vec![]).unwrap()),
            Err(Error::Sequence)
        ));
        assert!(matches!(
            b.verify(&snapshot, SignedControl::decode(&replay).unwrap()),
            Err(Error::Sequence)
        ));
        assert_eq!((a.tx, b.rx), (u64::MAX, u64::MAX));
    }

    #[test]
    fn session_policy_revocation_and_staleness_do_not_consume_state() {
        let snapshot = snapshot();
        let (mut a, mut b) = sessions(&snapshot);
        let signed = a.sign(&snapshot, Control::new(1, vec![]).unwrap()).unwrap();
        let (_, next) = crate::crypto::tests::trust(2);
        assert!(matches!(
            b.verify(&next, SignedControl::decode(signed.encode()).unwrap()),
            Err(Error::UnknownKey)
        ));
        assert_eq!(b.rx, 0);
        assert!(matches!(b.take_offer(&next), Err(Error::UnknownKey)));
        assert!(b.remote.is_some());
        let revoked = next;
        assert!(matches!(
            b.verify(&revoked, SignedControl::decode(signed.encode()).unwrap()),
            Err(Error::UnknownKey)
        ));
        assert!(matches!(
            a.sign(&revoked, Control::new(2, vec![]).unwrap()),
            Err(Error::UnknownKey)
        ));
        assert_eq!((a.tx, b.rx), (1, 0));
        b.verify(&snapshot, signed).unwrap();
    }

    #[test]
    fn live_rotation_pins_handshakes_drains_sessions_and_checks_current_trust() {
        use crate::signing::{Keys, bundle_tests::bundle};
        let root = std::env::temp_dir().join(format!("racer-auth-rotation-{}", std::process::id()));
        std::fs::create_dir_all(&root).unwrap();
        let write = |generation, active, trusted: &[u8]| {
            std::fs::write(
                root.join("bundle.json"),
                bundle(generation, active, trusted, true),
            )
            .unwrap();
        };
        write(1, 1, &[1, 2]);
        let mut provider = Keys::bundle(&root, true).unwrap().live();
        let snapshot = Snapshot::signed(UniverseId::new([9; 32]), provider.clone());
        let (initiator, hello) = start(&snapshot, None);
        let (responder, reply) = accept(&snapshot, hello, None);
        write(2, 2, &[1, 2]);
        provider.reload_bundle(&root, true).unwrap();
        let (mut a, finish) = initiator.finish(reply).unwrap();
        let mut b = responder.finish(finish).unwrap();
        assert!(!a.admitting(&snapshot));
        let frame = a
            .sign(&snapshot, Control::new(1, b"in-flight".to_vec()).unwrap())
            .unwrap();
        assert_eq!(b.verify(&snapshot, frame).unwrap().body(), b"in-flight");
        let (mut next_a, mut next_b) = sessions(&snapshot);
        assert!(next_a.admitting(&snapshot));
        a.drain.set(Some(crate::environment::now()));
        assert!(matches!(
            a.sign(&snapshot, Control::new(2, vec![]).unwrap()),
            Err(Error::Expired)
        ));
        write(3, 3, &[2, 3]);
        provider.reload_bundle(&root, true).unwrap();
        assert!(!b.healthy(&snapshot));
        let frame = next_a
            .sign(&snapshot, Control::new(1, vec![]).unwrap())
            .unwrap();
        next_b.verify(&snapshot, frame).unwrap();
        std::fs::remove_dir_all(root).unwrap();
    }
}
