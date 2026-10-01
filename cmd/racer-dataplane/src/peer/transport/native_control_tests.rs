//! Safety matrices for the closed, signed native payload control schema.
use super::*;

#[test]
fn session_admitted_setup_grant_completion_still_require_exact_transfer_and_phase() {
    use crate::security::connection::tests::{pair, signer};
    let (mut sender, mut receiver) = pair();
    let a = signer(&sender);
    let b = signer(&receiver);
    let scope = RequestScope::new(
        crate::model::RequestId([4; 16]),
        std::time::Instant::now() + Duration::from_secs(30),
    )
    .unwrap();
    let original = binding(&scope);
    for phase in [
        Phase::Accept,
        Phase::Offer,
        Phase::Setup,
        Phase::Ready,
        Phase::Grant,
        Phase::Complete,
        Phase::Done,
        Phase::Finish,
    ] {
        let extensions = phase
            .fields()
            .iter()
            .map(|name| extension(name, p::binary(&[1; 32]).into_bytes()))
            .collect();
        let control = original
            .sign(&a, b.node(), phase, &[9; 32], 0, extensions)
            .unwrap();
        let signed = sender.sign(frame(control).unwrap()).unwrap();
        let admitted = receiver.admit(signed).unwrap();
        let control = unframe(admitted, phase.response()).unwrap();
        let copy = || {
            crate::peer::protocol::decode_signed(
                &crate::peer::protocol::encode_signed(&control).unwrap(),
            )
            .unwrap()
        };
        let mut wrong = original.clone();
        wrong.transfer.0[0] ^= 1;
        assert!(
            wrong
                .verify(&b, a.node(), copy(), &[phase], &[9; 32], 0, &scope)
                .is_err()
        );
        assert!(
            original
                .verify(
                    &b,
                    a.node(),
                    copy(),
                    &[Phase::Fallback],
                    &[9; 32],
                    0,
                    &scope
                )
                .is_err()
        );
        original
            .verify(&b, a.node(), control, &[phase], &[9; 32], 0, &scope)
            .unwrap();
    }
}
fn binding(scope: &RequestScope) -> Binding {
    Binding {
        request: [1; 32],
        response: [2; 32],
        transfer: TransferId([3; 16]),
        membership: 1,
        deadline: p::encode_deadline(scope.deadline).unwrap(),
        rail: RailId(7),
    }
}
#[test]
fn exact_signed_controls_reject_every_binding_substitution_and_unknown_field() {
    let nodes = crate::peer::tests::signers();
    let scope = RequestScope::new(
        crate::model::RequestId([4; 16]),
        std::time::Instant::now() + Duration::from_secs(30),
    )
    .unwrap();
    let original = binding(&scope);
    for mutation in 0..9 {
        let signed = original
            .sign(
                &nodes[0],
                nodes[1].node(),
                Phase::Fallback,
                &[9; 32],
                0,
                vec![],
            )
            .unwrap();
        let mut expected = original.clone();
        match mutation {
            0 => expected.request[0] ^= 1,
            1 => expected.response[0] ^= 1,
            2 => expected.transfer.0[0] ^= 1,
            3 => expected.membership += 1,
            4 => expected.deadline += 1,
            5 => expected.rail.0 += 1,
            _ => {}
        }
        let previous = if mutation == 6 { [8; 32] } else { [9; 32] };
        let phase = if mutation == 7 {
            Phase::Done
        } else {
            Phase::Fallback
        };
        let peer = if mutation == 8 {
            nodes[2].node()
        } else {
            nodes[0].node()
        };
        assert!(
            expected
                .verify(&nodes[1], peer, signed, &[phase], &previous, 0, &scope)
                .is_err()
        );
    }
    let mut head = original.head(Phase::Fallback, &[9; 32], 0, vec![]).unwrap();
    p::push(&mut head, "racer-receiver", &nodes[1].node().0);
    p::push(&mut head, "racer-extra", 1);
    assert!(
        original
            .verify(
                &nodes[1],
                nodes[0].node(),
                nodes[0].sign(head).unwrap(),
                &[Phase::Fallback],
                &[9; 32],
                0,
                &scope
            )
            .is_err()
    );
    let signed = original
        .sign(
            &nodes[0],
            nodes[1].node(),
            Phase::Fallback,
            &[9; 32],
            0,
            vec![],
        )
        .unwrap();
    let copy = crate::peer::protocol::decode_signed(
        &crate::peer::protocol::encode_signed(&signed).unwrap(),
    )
    .unwrap();
    original
        .verify(
            &nodes[1],
            nodes[0].node(),
            signed,
            &[Phase::Fallback],
            &[9; 32],
            0,
            &scope,
        )
        .unwrap();
    original
        .verify(
            &nodes[1],
            nodes[0].node(),
            copy,
            &[Phase::Fallback],
            &[9; 32],
            0,
            &scope,
        )
        .unwrap();
    crate::security::connection::tests::replay_and_binding_checks();
}
#[test]
fn control_extension_and_fallback_length_schema_is_closed() {
    let scope = RequestScope::new(
        crate::model::RequestId([4; 16]),
        std::time::Instant::now() + Duration::from_secs(30),
    )
    .unwrap();
    let binding = binding(&scope);
    assert!(binding.head(Phase::Offer, &[0; 32], 0, vec![]).is_err());
    assert!(
        binding
            .head(
                Phase::Complete,
                &[0; 32],
                0,
                vec![extension("racer-rdma-completion", b"bad".to_vec())]
            )
            .is_err()
    );
    assert!(
        binding
            .head(
                Phase::Grant,
                &[0; 32],
                17,
                vec![extension(
                    "racer-rdma-descriptor",
                    p::binary(&[0; 32]).into_bytes()
                )]
            )
            .is_err()
    );
    assert!(binding.head(Phase::Finish, &[0; 32], 17, vec![]).is_ok());
    assert!(
        binding
            .head(Phase::Finish, &[0; 32], usize::MAX, vec![])
            .is_err()
    );
}
