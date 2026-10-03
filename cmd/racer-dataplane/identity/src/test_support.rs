use super::*;
pub(crate) fn ca() -> (rcgen::Certificate, rcgen::KeyPair) {
    let mut params = rcgen::CertificateParams::new(Vec::<String>::new()).unwrap();
    params.is_ca = rcgen::IsCa::Ca(rcgen::BasicConstraints::Unconstrained);
    params.key_usages = vec![
        rcgen::KeyUsagePurpose::KeyCertSign,
        rcgen::KeyUsagePurpose::CrlSign,
    ];
    let key = rcgen::KeyPair::generate_for(&rcgen::PKCS_ED25519).unwrap();
    (params.self_signed(&key).unwrap(), key)
}
pub(crate) fn issue(
    ca: &rcgen::Certificate,
    ca_key: &rcgen::KeyPair,
    cluster: &ClusterId,
    node: &NodeId,
    customize: impl FnOnce(&mut rcgen::CertificateParams),
) -> (PendingIdentity, Vec<Vec<u8>>) {
    let pending = PendingIdentity::generate().unwrap();
    let secret = pending.export_pkcs8_for_persistence().unwrap();
    let key = rcgen::KeyPair::from_pkcs8_der_and_sign_algo(
        &PrivatePkcs8KeyDer::from(secret.as_slice()),
        &rcgen::PKCS_ED25519,
    )
    .unwrap();
    let mut params = rcgen::CertificateParams::new(Vec::<String>::new()).unwrap();
    params.subject_alt_names = vec![rcgen::SanType::URI(
        format!("spiffe://{}/node/{}", cluster.0, node.0)
            .try_into()
            .unwrap(),
    )];
    params.key_usages = vec![rcgen::KeyUsagePurpose::DigitalSignature];
    params.extended_key_usages = vec![rcgen::ExtendedKeyUsagePurpose::ClientAuth];
    customize(&mut params);
    let cert = params.signed_by(&key, ca, ca_key).unwrap();
    (pending, vec![cert.der().to_vec()])
}
