//! Verified identities, signature chains, and separate page/credential AEAD domains.
pub mod aead;
pub mod credentials;

impl From<racer_identity::Error> for crate::error::Error {
    fn from(error: racer_identity::Error) -> Self {
        match error {
            racer_identity::Error::InvalidRequest => Self::InvalidRequest,
            racer_identity::Error::InvalidConfiguration => Self::InvalidConfiguration,
            racer_identity::Error::Unauthorized => Self::Unauthorized,
            racer_identity::Error::Unavailable => Self::Unavailable,
            racer_identity::Error::MissingKey => Self::MissingKey,
            racer_identity::Error::CorruptRecord => Self::CorruptRecord,
        }
    }
}

#[cfg(test)]
pub(crate) mod test_support {
    use crate::peer::protocol::Signatures;
    use crate::peer::protocol::SignedHead;
    use crate::http::Codec;
    use crate::model::CacheId;
    use crate::model::ClusterId;
    use crate::model::NodeId;
    use crate::peer::protocol;
    use racer_control_wire::BundleGeneration;
    use racer_control_wire::CacheEncryptionKey;
    use racer_control_wire::CacheKeyPurpose;
    use racer_control_wire::CacheKeyRef;
    use racer_control_wire::CacheKeyState;
    use racer_control_wire::KeyringBundle;
    use racer_control_wire::SCHEMA_VERSION;
    use racer_identity::Certificates;
    use racer_identity::KeyEpochs;
    use racer_identity::KeyLease;
    use racer_identity::Keyring;
    use racer_identity::PendingIdentity;
    use std::rc::Rc;
    use std::sync::Arc;
    pub struct Identity {
        pub keys: Rc<Keyring>,
        pub certificates: Rc<Certificates>,
        pub signatures: Rc<Signatures>,
    }
    pub(crate) const CLUSTER: &str = "11111111-1111-4111-8111-111111111111";
    pub(crate) const NODE: &str = "22222222-2222-4222-8222-222222222222";
    pub(crate) const CACHE: &str = "33333333-3333-4333-8333-333333333333";
    pub(crate) fn issued() -> (PendingIdentity, Vec<Vec<u8>>, Vec<Vec<u8>>) {
        let (ca, ca_key) = ca();
        let (pending, chain) = issue(
            &ca,
            &ca_key,
            &ClusterId(CLUSTER.into()),
            &NodeId(NODE.into()),
            |_| {},
        );
        (pending, chain, vec![ca.der().to_vec()])
    }
    pub(crate) fn keys() -> Keyring {
        keys_for(&[CacheId(CACHE.into())])
    }
    pub(crate) fn assert_page_key(lease: &KeyLease, expected: &[u8; 32]) {
        let mut sealed = [0; 19];
        lease
            .seal_page(lease.cache(), &[1; 24], b"retained", b"abc", &mut sealed)
            .unwrap();
        let mut opened = [0; 3];
        racer_crypto::aead::open(expected, &[1; 24], b"retained", &sealed, &mut opened).unwrap();
        assert_eq!(&opened, b"abc");
    }
    pub(crate) fn keys_for(caches: &[CacheId]) -> Keyring {
        let (_, _, roots) = issued();
        let keys = Keyring::new(
            ClusterId(CLUSTER.into()),
            NodeId(NODE.into()),
            Arc::new(KeyEpochs::default()),
        );
        let mut initial = rotation_bundle(1, roots);
        // keys() historically used the unrotated seed bundle; rotation_bundle(1)
        // intentionally has different suffixes and material just like later epochs.
        for (i, record) in initial.cache_keys.iter_mut().enumerate() {
            record.key.id = crate::model::key_id_from_generation(1, i as u32 + 1).unwrap();
            *record = CacheEncryptionKey::new(
                record.key.clone(),
                record.state,
                zeroize::Zeroizing::new([7 + i as u8; 32]),
            );
        }
        let templates = std::mem::take(&mut initial.cache_keys);
        for (i, cache) in caches.iter().enumerate() {
            for template in &templates {
                let (mut key, state, mut material) = template.clone().into_installation();
                key.cache = cache.clone();
                if i != 0 {
                    material[..8].copy_from_slice(&(i as u64).to_be_bytes());
                }
                initial
                    .cache_keys
                    .push(CacheEncryptionKey::new(key, state, material));
            }
        }
        keys.install(initial).unwrap();
        keys
    }
    pub(crate) fn rotation_bundle(generation: u64, roots: Vec<Vec<u8>>) -> KeyringBundle {
        KeyringBundle {
            schema_version: SCHEMA_VERSION,
            cluster: ClusterId(CLUSTER.into()),
            generation: BundleGeneration(generation),
            peer_trust_roots: roots,
            cache_keys: [CacheKeyPurpose::Page, CacheKeyPurpose::OriginCredentials]
                .into_iter()
                .enumerate()
                .map(|(i, purpose)| {
                    let mut material = zeroize::Zeroizing::new([7 + i as u8; 32]);
                    material[..8].copy_from_slice(&generation.to_be_bytes());
                    CacheEncryptionKey::new(
                        CacheKeyRef {
                            cache: CacheId(CACHE.into()),
                            id: crate::model::key_id_from_generation(generation, i as u32).unwrap(),
                            purpose,
                        },
                        CacheKeyState::Active,
                        material,
                    )
                })
                .collect(),
        }
    }
    pub(crate) fn node(n: usize) -> NodeId {
        NodeId(format!("{n:08x}-1111-4111-8111-111111111111"))
    }
    pub(crate) fn network(count: usize) -> Vec<Rc<Signatures>> {
        identities(
            ClusterId(node(99).0),
            &(0..count).map(node).collect::<Vec<_>>(),
            mac_test_keys,
        )
        .into_iter()
        .map(|identity| identity.signatures)
        .collect()
    }
    pub(crate) fn mac_test_keys() -> Vec<CacheEncryptionKey> {
        [node(88).0, CACHE.into()]
            .into_iter()
            .enumerate()
            .map(|(i, cache)| {
                CacheEncryptionKey::new(
                    CacheKeyRef {
                        cache: CacheId(cache),
                        id: crate::model::key_id_from_generation(1, 100 + i as u32).unwrap(),
                        purpose: CacheKeyPurpose::OriginCredentials,
                    },
                    CacheKeyState::Active,
                    zeroize::Zeroizing::new([100 + i as u8; 32]),
                )
            })
            .collect()
    }
    pub(crate) fn mac_test_key(cache: &str) -> Vec<CacheEncryptionKey> {
        let mut keys = mac_test_keys();
        keys.truncate(1);
        keys[0].key.cache = CacheId(cache.into());
        keys
    }
    pub(crate) fn clone_head(head: &SignedHead) -> SignedHead {
        let codec = Codec::new(protocol::MAX_HEAD);
        let encoded = codec.encode_head(&head.head).unwrap();
        SignedHead {
            head: codec.decode_head(&encoded).unwrap().unwrap().0,
            signature: head.signature.clone(),
        }
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
    use racer_identity::KeyPurpose;
    #[test]
    fn complete_component_error_mapping_preserves_application_meanings() {
        use crate::error::Error as App;
        use racer_identity::Error as Identity;
        for (component, application) in [
            (Identity::InvalidRequest, App::InvalidRequest),
            (Identity::InvalidConfiguration, App::InvalidConfiguration),
            (Identity::Unauthorized, App::Unauthorized),
            (Identity::Unavailable, App::Unavailable),
            (Identity::MissingKey, App::MissingKey),
            (Identity::CorruptRecord, App::CorruptRecord),
        ] {
            assert_eq!(App::from(component), application);
        }
    }
    #[test]
    fn request_key_purpose_separation() {
        let keys = test_support::keys();
        let cache = crate::model::CacheId(test_support::CACHE.into());
        let mut tag = [0; 32];
        assert!(
            keys.active(&cache, KeyPurpose::Page)
                .unwrap()
                .request_mac(&cache, b"request", &mut tag)
                .is_err()
        );
        let credential = keys.active(&cache, KeyPurpose::OriginCredentials).unwrap();
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
