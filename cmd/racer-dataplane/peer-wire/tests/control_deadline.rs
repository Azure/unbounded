use http1::MessageHead;
use peer_wire::{
    Error, RequestMac, Result, Signatures,
    control::{Binding, Phase},
    decode_deadline, encode_deadline,
};
use racer_control_wire::{
    BundleGeneration, ClusterId, KeyringBundle, NodeId, RailId, SCHEMA_VERSION,
};
use racer_crypto::identity::{
    Certificates, KeyEpochs, Keyring,
    test_util::{ca, issue},
};
use racer_platform::environment::{Deadline, SimulationClock, now};
use std::{
    rc::Rc,
    sync::Arc,
    time::{Duration, Instant, UNIX_EPOCH},
};

struct NoMac;

impl RequestMac for NoMac {
    fn sign(&self, _: &Keyring, _: &mut MessageHead) -> Result<()> {
        Ok(())
    }

    fn verify(&self, _: &Keyring, _: &MessageHead) -> Result<()> {
        Ok(())
    }
}

fn signatures() -> Signatures {
    let (ca, key) = ca();
    let roots = vec![ca.der().to_vec()];
    let cluster = ClusterId("00000099-1111-4111-8111-111111111111".into());
    let node = NodeId("00000001-1111-4111-8111-111111111111".into());
    let (pending, chain) = issue(&ca, &key, &cluster, &node, |_| {});
    let identity = pending
        .accept(cluster.clone(), node.clone(), chain, &roots)
        .unwrap();
    let keys = Rc::new(Keyring::new(
        cluster.clone(),
        node,
        Arc::new(KeyEpochs::default()),
    ));
    keys.install(KeyringBundle {
        schema_version: SCHEMA_VERSION,
        cluster: cluster.clone(),
        generation: BundleGeneration(1),
        peer_trust_roots: roots,
        cache_keys: vec![],
    })
    .unwrap();
    keys.install_signing_identity(Arc::new(identity)).unwrap();
    let certificates = Rc::new(Certificates::new(cluster, keys.clone()));
    Signatures::new(keys, certificates, Rc::new(NoMac))
}

fn binding(deadline: u64) -> Binding {
    Binding {
        request: [1; 32],
        response: [2; 32],
        transfer: [3; 16],
        membership: 1,
        deadline,
        rail: RailId(1),
    }
}

#[test]
fn head_and_sign_reject_unmappable_deadlines() {
    let signatures = signatures();
    let origin = Instant::now();
    let (mut low, mut high) = (0, u64::MAX);
    while low < high {
        let seconds = low + (high - low).div_ceil(2);
        if origin.checked_add(Duration::from_secs(seconds)).is_some() {
            low = seconds;
        } else {
            high = seconds - 1;
        }
    }
    let wall_seconds = 1_800_000_000;
    let clock = SimulationClock::new_at(
        1,
        origin + Duration::from_secs(low),
        UNIX_EPOCH + Duration::from_secs(wall_seconds),
    );
    let _environment = clock.environment(0).enter();
    binding(wall_seconds * 1000)
        .sign(
            &signatures,
            signatures.node(),
            Phase::Accept,
            &[0; 32],
            0,
            vec![],
        )
        .unwrap();
    for deadline in [(wall_seconds + 1) * 1000, u64::MAX] {
        assert!(matches!(
            decode_deadline(deadline),
            Err(Error::Runtime(racer_platform::Error::InvalidInput))
        ));
        let binding = binding(deadline);
        assert!(matches!(
            binding.head(Phase::Accept, &[0; 32], 0, vec![]),
            Err(Error::Runtime(racer_platform::Error::InvalidInput))
        ));
        assert!(matches!(
            binding.sign(
                &signatures,
                signatures.node(),
                Phase::Accept,
                &[0; 32],
                0,
                vec![]
            ),
            Err(Error::Runtime(racer_platform::Error::InvalidInput))
        ));
    }
}

#[test]
fn representable_deadline_boundaries_preserve_verification_policy() {
    let signatures = signatures();
    for deadline in [0, u64::MAX] {
        assert_eq!(
            encode_deadline(decode_deadline(deadline).unwrap()),
            Ok(deadline)
        );
        let binding = binding(deadline);
        let head = binding.head(Phase::Accept, &[0; 32], 0, vec![]).unwrap();
        assert_eq!(
            head.unique("racer-payload-deadline").unwrap(),
            Some(deadline.to_string().as_bytes())
        );
        let signed = binding
            .sign(
                &signatures,
                signatures.node(),
                Phase::Accept,
                &[0; 32],
                0,
                vec![],
            )
            .unwrap();
        let verified = binding.verify(
            &signatures,
            signatures.node(),
            signed,
            &[Phase::Accept],
            &[0; 32],
            0,
        );
        if deadline == 0 {
            assert!(matches!(verified, Err(Error::DeadlineExceeded)));
        } else {
            assert_eq!(verified.unwrap().1, Phase::Accept);
        }
    }
}

#[test]
fn future_deadline_signs_and_verifies() {
    let signatures = signatures();
    let binding = binding(encode_deadline(Deadline(now() + Duration::from_secs(30))).unwrap());
    let signed = binding
        .sign(
            &signatures,
            signatures.node(),
            Phase::Accept,
            &[0; 32],
            0,
            vec![],
        )
        .unwrap();
    assert_eq!(Binding::parse_accept(&signed).unwrap(), binding);
    assert_eq!(
        binding
            .verify(
                &signatures,
                signatures.node(),
                signed,
                &[Phase::Accept],
                &[0; 32],
                0
            )
            .unwrap()
            .1,
        Phase::Accept
    );
}
