//! Opt-in certificate fixtures. Defaults match the original identity tests.
use super::*;
pub fn ca() -> (rcgen::Certificate, rcgen::KeyPair) {
    let mut params = rcgen::CertificateParams::new(Vec::<String>::new()).unwrap();
    params.is_ca = rcgen::IsCa::Ca(rcgen::BasicConstraints::Unconstrained);
    params.key_usages = vec![
        rcgen::KeyUsagePurpose::KeyCertSign,
        rcgen::KeyUsagePurpose::CrlSign,
    ];
    let key = rcgen::KeyPair::generate_for(&rcgen::PKCS_ED25519).unwrap();
    (params.self_signed(&key).unwrap(), key)
}
pub fn issue(
    ca: &rcgen::Certificate,
    ca_key: &rcgen::KeyPair,
    cluster: &ClusterId,
    node: &NodeId,
    customize: impl FnOnce(&mut rcgen::CertificateParams),
) -> (PendingIdentity, Vec<Vec<u8>>) {
    let pending = PendingIdentity::generate().unwrap();
    issue_pending(pending, ca, ca_key, cluster, node, customize)
}

/// Issue for an existing key without regenerating it or changing certificate defaults.
pub fn issue_pending(
    pending: PendingIdentity,
    ca: &rcgen::Certificate,
    ca_key: &rcgen::KeyPair,
    cluster: &ClusterId,
    node: &NodeId,
    customize: impl FnOnce(&mut rcgen::CertificateParams),
) -> (PendingIdentity, Vec<Vec<u8>>) {
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

#[cfg(test)]
mod tests {
    use super::*;
    use std::time::{Duration, UNIX_EPOCH};

    #[test]
    fn tls_time_uses_current_scoped_wall_clock_clamps_and_truncates() {
        let clock = uring_runtime::environment::SimulationClock::new(71);
        let _environment = clock.environment(0).enter();
        for (wall, seconds) in [
            (UNIX_EPOCH - Duration::from_nanos(1), 0),
            (UNIX_EPOCH, 0),
            (UNIX_EPOCH + Duration::from_millis(1999), 1),
            (UNIX_EPOCH + Duration::from_secs(10), 10),
        ] {
            clock.set_wall_time(wall);
            assert_eq!(unix_time().as_secs(), seconds);
        }
    }

    #[test]
    fn existing_key_and_customized_certificate_contract_are_preserved() {
        let (ca, ca_key) = ca();
        let cluster = ClusterId("11111111-1111-4111-8111-111111111111".into());
        let node = NodeId("22222222-2222-4222-8222-222222222222".into());
        let pending = PendingIdentity::generate().unwrap();
        let original = pending.export_pkcs8_for_persistence().unwrap();
        let (pending, chain) = issue_pending(pending, &ca, &ca_key, &cluster, &node, |params| {
            params.not_before = rcgen::date_time_ymd(2020, 1, 1);
            params.not_after = rcgen::date_time_ymd(2030, 1, 1);
        });
        assert_eq!(*pending.export_pkcs8_for_persistence().unwrap(), *original);
        let (_, cert) = parse_x509_certificate(&chain[0]).unwrap();
        assert_eq!(cert.validity().not_before.timestamp(), 1_577_836_800);
        assert_eq!(cert.validity().not_after.timestamp(), 1_893_456_000);
        pending
            .accept(cluster, node, chain, &[ca.der().to_vec()])
            .unwrap();
    }
}
