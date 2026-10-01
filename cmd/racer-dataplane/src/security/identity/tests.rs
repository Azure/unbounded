use super::*;
pub(crate) const CLUSTER: &str = "11111111-1111-4111-8111-111111111111";
pub(crate) const NODE: &str = "22222222-2222-4222-8222-222222222222";
pub(crate) const CACHE: &str = "33333333-3333-4333-8333-333333333333";
pub(crate) fn issued() -> (PendingIdentity, Vec<Vec<u8>>, Vec<Vec<u8>>) {
    issued_with(|_| {})
}
pub(crate) fn issued_with(
    customize: impl FnOnce(&mut rcgen::CertificateParams),
) -> (PendingIdentity, Vec<Vec<u8>>, Vec<Vec<u8>>) {
    let mut ca_params = rcgen::CertificateParams::new(Vec::<String>::new()).unwrap();
    ca_params.is_ca = rcgen::IsCa::Ca(rcgen::BasicConstraints::Unconstrained);
    ca_params.key_usages = vec![
        rcgen::KeyUsagePurpose::KeyCertSign,
        rcgen::KeyUsagePurpose::CrlSign,
    ];
    let ca_key = rcgen::KeyPair::generate_for(&rcgen::PKCS_ED25519).unwrap();
    let ca = ca_params.self_signed(&ca_key).unwrap();
    let pending = PendingIdentity::generate().unwrap();
    let bytes = pending.export_pkcs8_for_persistence().unwrap();
    let key = rcgen::KeyPair::from_pkcs8_der_and_sign_algo(
        &PrivatePkcs8KeyDer::from(bytes.as_slice()),
        &rcgen::PKCS_ED25519,
    )
    .unwrap();
    let mut params = rcgen::CertificateParams::new(Vec::<String>::new()).unwrap();
    params.subject_alt_names = vec![rcgen::SanType::URI(
        format!("spiffe://{CLUSTER}/node/{NODE}")
            .try_into()
            .unwrap(),
    )];
    params.key_usages = vec![rcgen::KeyUsagePurpose::DigitalSignature];
    params.extended_key_usages = vec![rcgen::ExtendedKeyUsagePurpose::ClientAuth];
    customize(&mut params);
    let cert = params.signed_by(&key, &ca, &ca_key).unwrap();
    (pending, vec![cert.der().to_vec()], vec![ca.der().to_vec()])
}
#[test]
fn identity_recovery_csr_tls_and_key_pairing() {
    let (pending, chain, roots) = issued();
    assert!(!pending.csr_der().unwrap().is_empty());
    let bytes = pending.export_pkcs8_for_persistence().unwrap();
    let identity = pending
        .accept(
            ClusterId(CLUSTER.into()),
            NodeId(NODE.into()),
            chain.clone(),
            &roots,
        )
        .unwrap();
    assert!(identity.tls_certified_key().is_ok());
    let recovered = SigningIdentity::from_pkcs8(
        identity.cluster.clone(),
        identity.node.clone(),
        &bytes,
        chain.clone(),
        &roots,
    )
    .unwrap();
    assert_eq!(
        identity.sign(b"exact message").unwrap(),
        recovered.sign(b"exact message").unwrap()
    );
    assert!(
        PendingIdentity::generate()
            .unwrap()
            .accept(
                identity.cluster.clone(),
                identity.node.clone(),
                chain.clone(),
                &roots,
            )
            .is_err()
    );
    assert!(
        SigningIdentity::from_pkcs8(
            identity.cluster.clone(),
            NodeId("other".into()),
            &bytes,
            chain,
            &roots,
        )
        .is_err()
    );
}
