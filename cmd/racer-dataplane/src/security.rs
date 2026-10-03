//! Verified identities, signature chains, and separate page/credential AEAD domains.
pub mod aead;
pub mod connection;
pub mod credentials;
#[cfg(test)]
pub(crate) mod fixtures;
pub mod forwarding;
pub mod identity;
pub mod protocol;
#[cfg(test)]
pub(crate) mod test_support {
    use super::{
        connection::Signatures,
        identity::{Certificates, KeyEpochs, Keyring, PendingIdentity},
    };
    use crate::{
        control::wire::{BundleGeneration, CacheEncryptionKey, KeyringBundle, SCHEMA_VERSION},
        model::{ClusterId, NodeId},
    };
    use std::{rc::Rc, sync::Arc};
    pub struct Identity {
        pub keys: Rc<Keyring>,
        pub certificates: Rc<Certificates>,
        pub signatures: Rc<Signatures>,
    }
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
            &rustls::pki_types::PrivatePkcs8KeyDer::from(secret.as_slice()),
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
    pub fn identities(
        cluster: ClusterId,
        nodes: &[NodeId],
        cache_keys: impl Fn() -> Vec<CacheEncryptionKey>,
    ) -> Vec<Identity> {
        let (ca, ca_key) = ca();
        let roots = vec![ca.der().to_vec()];
        nodes
            .iter()
            .map(|node| {
                let (pending, chain) = issue(&ca, &ca_key, &cluster, node, |_| {});
                let identity = pending
                    .accept(cluster.clone(), node.clone(), chain, &roots)
                    .unwrap();
                let keys = Rc::new(Keyring::new(
                    cluster.clone(),
                    node.clone(),
                    Arc::new(KeyEpochs::default()),
                ));
                keys.install(KeyringBundle {
                    schema_version: SCHEMA_VERSION,
                    cluster: cluster.clone(),
                    generation: BundleGeneration(1),
                    peer_trust_roots: roots.clone(),
                    cache_keys: cache_keys(),
                })
                .unwrap();
                keys.install_signing_identity(Arc::new(identity)).unwrap();
                let certificates = Rc::new(Certificates::new(cluster.clone(), keys.clone()));
                let signatures = Rc::new(Signatures::new(keys.clone(), certificates.clone()));
                Identity {
                    keys,
                    certificates,
                    signatures,
                }
            })
            .collect()
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn request_key_purpose_separation() {
        let keys = identity::keyring_tests::keys();
        let cache = crate::model::CacheId(identity::tests::CACHE.into());
        let mut tag = [0; 32];
        assert!(
            keys.active(&cache, identity::KeyPurpose::Page)
                .unwrap()
                .request_mac(&cache, b"request", &mut tag)
                .is_err()
        );
        let credential = keys
            .active(&cache, identity::KeyPurpose::OriginCredentials)
            .unwrap();
        credential
            .request_mac(&cache, b"request", &mut tag)
            .unwrap();
        credential
            .verify_request_mac(&cache, credential.id(), b"request", &tag)
            .unwrap();
        assert!(
            credential
                .verify_request_mac(&cache, credential.id(), b"changed", &tag)
                .is_err()
        );
    }
}
