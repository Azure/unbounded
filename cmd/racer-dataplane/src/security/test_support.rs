//! Shared CA, node identity, keyring, and signer fixture for signed scenarios.
use super::{
    identity::Certificates,
    identity::PendingIdentity,
    identity::{KeyEpochs, Keyring},
    signing::Signatures,
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

pub fn identities(
    cluster: ClusterId,
    nodes: &[NodeId],
    cache_keys: impl Fn() -> Vec<CacheEncryptionKey>,
) -> Vec<Identity> {
    let mut params = rcgen::CertificateParams::new(Vec::<String>::new()).unwrap();
    params.is_ca = rcgen::IsCa::Ca(rcgen::BasicConstraints::Unconstrained);
    params.key_usages = vec![
        rcgen::KeyUsagePurpose::KeyCertSign,
        rcgen::KeyUsagePurpose::CrlSign,
    ];
    let ca_key = rcgen::KeyPair::generate_for(&rcgen::PKCS_ED25519).unwrap();
    let ca = params.self_signed(&ca_key).unwrap();
    let roots = vec![ca.der().to_vec()];
    nodes
        .iter()
        .map(|node| {
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
            let cert = params.signed_by(&key, &ca, &ca_key).unwrap();
            let identity = pending
                .accept(
                    cluster.clone(),
                    node.clone(),
                    vec![cert.der().to_vec()],
                    &roots,
                )
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
