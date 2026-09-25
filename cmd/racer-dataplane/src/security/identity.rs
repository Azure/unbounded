//! Locally generated Ed25519 identities. Secret export is explicit and zeroizing.
use super::certificates::{spiffe, verify_chain};
use crate::{
    error::{Error, Result},
    model::identity::{ClusterId, NodeId},
};
use ed25519_dalek::{
    Signer, SigningKey,
    pkcs8::{DecodePrivateKey, EncodePrivateKey},
};
use rustls::pki_types::{CertificateDer, PrivateKeyDer, PrivatePkcs8KeyDer};
use zeroize::Zeroizing;

pub struct PendingIdentity {
    key: SigningKey,
}
pub struct SigningIdentity {
    cluster: ClusterId,
    node: NodeId,
    key: SigningKey,
    chain: Vec<Vec<u8>>,
}
impl PendingIdentity {
    pub fn generate() -> Result<Self> {
        let mut seed = Zeroizing::new([0u8; 32]);
        getrandom::getrandom(&mut *seed).map_err(|_| Error::Unavailable)?;
        Ok(Self {
            key: SigningKey::from_bytes(&seed),
        })
    }
    pub fn recover(pkcs8: &[u8]) -> Result<Self> {
        if pkcs8.len() > 4096 {
            return Err(Error::InvalidRequest);
        }
        Ok(Self {
            key: SigningKey::from_pkcs8_der(pkcs8).map_err(|_| Error::Unauthorized)?,
        })
    }
    pub fn export_pkcs8_for_persistence(&self) -> Result<Zeroizing<Vec<u8>>> {
        Ok(Zeroizing::new(
            self.key
                .to_pkcs8_der()
                .map_err(|_| Error::Unavailable)?
                .as_bytes()
                .to_vec(),
        ))
    }
    /// The control server assigns the node SAN from authenticated enrollment.
    pub fn csr_der(&self) -> Result<Vec<u8>> {
        let bytes = self.export_pkcs8_for_persistence()?;
        let der = PrivatePkcs8KeyDer::from(bytes.as_slice());
        let key = rcgen::KeyPair::from_pkcs8_der_and_sign_algo(&der, &rcgen::PKCS_ED25519)
            .map_err(|_| Error::Unavailable)?;
        let params =
            rcgen::CertificateParams::new(Vec::<String>::new()).map_err(|_| Error::Unavailable)?;
        Ok(params
            .serialize_request(&key)
            .map_err(|_| Error::Unavailable)?
            .der()
            .to_vec())
    }
    pub fn accept(
        self,
        cluster: ClusterId,
        node: NodeId,
        chain: Vec<Vec<u8>>,
        roots: &[Vec<u8>],
    ) -> Result<SigningIdentity> {
        let public = verify_chain(roots, &chain, &cluster, &node)?;
        if public != self.key.verifying_key() {
            return Err(Error::Unauthorized);
        }
        Ok(SigningIdentity {
            cluster,
            node,
            key: self.key,
            chain,
        })
    }
}
#[cfg(test)]
pub(crate) mod tests {
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
                    &roots
                )
                .is_err()
        );
        assert!(
            SigningIdentity::from_pkcs8(
                identity.cluster.clone(),
                NodeId("other".into()),
                &bytes,
                chain,
                &roots
            )
            .is_err()
        );
    }
}
impl SigningIdentity {
    pub fn from_pkcs8(
        cluster: ClusterId,
        node: NodeId,
        pkcs8: &[u8],
        chain: Vec<Vec<u8>>,
        roots: &[Vec<u8>],
    ) -> Result<Self> {
        PendingIdentity::recover(pkcs8)?.accept(cluster, node, chain, roots)
    }
    pub fn cluster(&self) -> &ClusterId {
        &self.cluster
    }
    pub fn node(&self) -> &NodeId {
        &self.node
    }
    pub fn certificate_chain(&self) -> &[Vec<u8>] {
        &self.chain
    }
    pub fn sign(&self, message: &[u8]) -> Result<Vec<u8>> {
        let (_, leaf) =
            x509_parser::parse_x509_certificate(&self.chain[0]).map_err(|_| Error::Unauthorized)?;
        if !leaf.validity().is_valid() {
            return Err(Error::Unauthorized);
        }
        Ok(self.key.sign(message).to_bytes().to_vec())
    }
    pub fn spiffe_id(&self) -> Result<String> {
        spiffe(&self.cluster, &self.node)
    }
    pub fn export_pkcs8_for_persistence(&self) -> Result<Zeroizing<Vec<u8>>> {
        Ok(Zeroizing::new(
            self.key
                .to_pkcs8_der()
                .map_err(|_| Error::Unavailable)?
                .as_bytes()
                .to_vec(),
        ))
    }
    pub fn tls_certified_key(&self) -> Result<rustls::sign::CertifiedKey> {
        let bytes = self.export_pkcs8_for_persistence()?;
        let private = PrivateKeyDer::Pkcs8(PrivatePkcs8KeyDer::from(bytes.as_slice()));
        let key = rustls::crypto::ring::sign::any_supported_type(&private)
            .map_err(|_| Error::Unauthorized)?;
        let certified = rustls::sign::CertifiedKey::new(
            self.chain
                .iter()
                .cloned()
                .map(CertificateDer::from)
                .collect(),
            key,
        );
        certified.keys_match().map_err(|_| Error::Unauthorized)?;
        Ok(certified)
    }
}
