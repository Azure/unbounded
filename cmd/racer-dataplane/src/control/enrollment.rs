//! Generate node-private Ed25519 keys locally and enroll/rotate with node-bound SA identity.
use super::wire::{EnrollmentId, EnrollmentRequest, EnrollmentResponse};
use super::{files, wire};
use crate::{
    error::{Error, Operation, Result},
    model::identity::{ClusterId, NodeId},
    runtime::deadline::RequestScope,
};
use base64::{Engine, engine::general_purpose::STANDARD};
use serde::{Deserialize, Serialize};
use std::{
    cell::RefCell,
    path::PathBuf,
    sync::Arc,
    time::{Duration, SystemTime, UNIX_EPOCH},
};
use zeroize::{Zeroize, Zeroizing};
pub struct Enrollment {
    cluster: ClusterId,
    token_path: PathBuf,
    identity_directory: PathBuf,
    roots: RefCell<Vec<Vec<u8>>>,
}
/// Non-exportable signing identity. Do not share private keys through cluster Secrets.
#[derive(Clone)]
pub struct LocalSigningIdentity {
    cluster: ClusterId,
    node: NodeId,
    enrollment: EnrollmentId,
    private_material: Vec<u8>,
    certificate_chain: Vec<Vec<u8>>,
    not_before: u64,
    not_after: u64,
}
impl Drop for LocalSigningIdentity {
    fn drop(&mut self) {
        self.private_material.zeroize();
    }
}
#[derive(Serialize, Deserialize)]
struct PendingIdentity {
    cluster: String,
    enrollment: String,
    private_key: String,
    csr: String,
}
impl Drop for PendingIdentity {
    fn drop(&mut self) {
        self.private_key.zeroize();
    }
}
#[derive(Serialize, Deserialize)]
struct PersistedIdentity {
    pending: PendingIdentity,
    response: String,
}
impl Enrollment {
    pub fn new(cluster: ClusterId, token_path: PathBuf, identity_directory: PathBuf) -> Self {
        Self {
            cluster,
            token_path,
            identity_directory,
            roots: RefCell::new(Vec::new()),
        }
    }
    /// Persist a fresh private key and retry-stable request before submission.
    /// All issuance uses the projected token, including renewal at 16 hours.
    pub fn prepare<'a>(&'a self, scope: &'a RequestScope) -> Operation<'a, EnrollmentRequest> {
        Box::pin(async move {
            scope.check()?;
            self.prepare_now()
        })
    }
    pub fn cluster(&self) -> &ClusterId {
        &self.cluster
    }
    pub fn set_peer_trust_roots(&self, roots: Vec<Vec<u8>>) -> Result<()> {
        verifier(&roots)?;
        *self.roots.borrow_mut() = roots;
        Ok(())
    }
    /// Reread the projected token on every attempt; never retain it in a DTO.
    pub fn read_token(&self) -> Result<Zeroizing<String>> {
        // Kubernetes token paths normally pass through a projection symlink. Opening
        // once pins one complete token file across atomic directory replacement.
        let b = Zeroizing::new(files::read_path(
            &self.token_path,
            wire::MAX_ENROLLMENT_BYTES,
        )?);
        let token = Zeroizing::new(
            std::str::from_utf8(&b)
                .map_err(|_| Error::Unauthorized)?
                .trim()
                .to_owned(),
        );
        if token.is_empty()
            || !token
                .bytes()
                .all(|b| b.is_ascii_alphanumeric() || matches!(b, b'-' | b'_' | b'.'))
        {
            return Err(Error::Unauthorized);
        }
        Ok(token)
    }
    pub fn prepare_now(&self) -> Result<EnrollmentRequest> {
        if !wire::valid_uuid(&self.cluster.0) {
            return Err(Error::InvalidConfiguration);
        }
        let dir = files::directory(&self.identity_directory, true, true)?;
        match files::read_at(&dir, "pending.json", wire::MAX_ENROLLMENT_BYTES, true) {
            Ok(b) => return self.request(&decode_pending(&Zeroizing::new(b))?),
            Err(Error::MissingKey) => (),
            Err(e) => return Err(e),
        }
        let key = rcgen::KeyPair::generate_for(&rcgen::PKCS_ED25519).map_err(|_| Error::Io)?;
        let mut params = rcgen::CertificateParams::new(Vec::<String>::new())
            .map_err(|_| Error::InvalidRequest)?;
        params.distinguished_name = rcgen::DistinguishedName::new();
        let csr = params.serialize_request(&key).map_err(|_| Error::Io)?;
        let mut id = [0; 16];
        getrandom::getrandom(&mut id).map_err(|_| Error::Io)?;
        id[6] = (id[6] & 0x0f) | 0x40;
        id[8] = (id[8] & 0x3f) | 0x80;
        let h = format!("{:032x}", u128::from_be_bytes(id));
        let enrollment = format!(
            "{}-{}-{}-{}-{}",
            &h[..8],
            &h[8..12],
            &h[12..16],
            &h[16..20],
            &h[20..]
        );
        let pending = PendingIdentity {
            cluster: self.cluster.0.clone(),
            enrollment,
            private_key: STANDARD.encode(key.serialize_der()),
            csr: STANDARD.encode(csr.der()),
        };
        let encoded = Zeroizing::new(serde_json::to_vec(&pending).map_err(|_| Error::Io)?);
        files::atomic_write(&dir, "pending.json", &encoded)?;
        self.request(&pending)
    }
    fn request(&self, p: &PendingIdentity) -> Result<EnrollmentRequest> {
        if p.cluster != self.cluster.0 {
            return Err(Error::Unauthorized);
        }
        let r = EnrollmentRequest {
            schema_version: 1,
            cluster: self.cluster.clone(),
            enrollment: EnrollmentId(p.enrollment.clone()),
            csr_der: STANDARD.decode(&p.csr).map_err(|_| Error::CorruptRecord)?,
        };
        wire::encode_enrollment_request(&r)?;
        // A corrupt pending key must never be submitted, even if its CSR parses.
        use ed25519_dalek::pkcs8::DecodePrivateKey;
        use x509_parser::prelude::FromDer;
        let secret = Zeroizing::new(
            STANDARD
                .decode(&p.private_key)
                .map_err(|_| Error::CorruptRecord)?,
        );
        let key =
            ed25519_dalek::SigningKey::from_pkcs8_der(&secret).map_err(|_| Error::CorruptRecord)?;
        let (_, csr) =
            x509_parser::certification_request::X509CertificationRequest::from_der(&r.csr_der)
                .map_err(|_| Error::CorruptRecord)?;
        if csr
            .certification_request_info
            .subject_pki
            .subject_public_key
            .data
            .as_ref()
            != key.verifying_key().as_bytes()
        {
            return Err(Error::Unauthorized);
        }
        csr.verify_signature().map_err(|_| Error::Unauthorized)?;
        Ok(r)
    }
    /// Verify server-authenticated response correlation, chain/SAN/validity, and
    /// local key pairing, then persist the node identity. Never trust CSR SANs.
    pub fn accept_response(&self, response: EnrollmentResponse) -> Result<LocalSigningIdentity> {
        let dir = files::directory(&self.identity_directory, false, true)?;
        match files::read_at(&dir, "identity.json", wire::MAX_ENROLLMENT_BYTES * 3, true) {
            Ok(bytes) => {
                let bytes = Zeroizing::new(bytes);
                let old: PersistedIdentity = serde_json::from_value(wire::strict_json(
                    &bytes,
                    wire::MAX_ENROLLMENT_BYTES * 3,
                )?)
                .map_err(|_| Error::CorruptRecord)?;
                let old = wire::decode_enrollment_response(
                    &STANDARD
                        .decode(&old.response)
                        .map_err(|_| Error::CorruptRecord)?,
                )?;
                if old.cluster != response.cluster || old.node != response.node {
                    return Err(Error::Unauthorized);
                }
            }
            Err(Error::MissingKey) => (),
            Err(e) => return Err(e),
        }
        let pending = decode_pending(&Zeroizing::new(files::read_at(
            &dir,
            "pending.json",
            wire::MAX_ENROLLMENT_BYTES,
            true,
        )?))?;
        self.request(&pending)?;
        let identity = self.validate(&pending, &response)?;
        let persisted = PersistedIdentity {
            pending,
            response: STANDARD.encode(wire::encode_enrollment_response(&response)?),
        };
        let b = Zeroizing::new(serde_json::to_vec(&persisted).map_err(|_| Error::Io)?);
        files::atomic_write(&dir, "identity.json", &b)?;
        files::remove(&dir, "pending.json")?;
        Ok(identity)
    }
    pub fn load_identity(&self) -> Result<Option<LocalSigningIdentity>> {
        let dir = match files::directory(&self.identity_directory, false, true) {
            Ok(d) => d,
            Err(Error::MissingKey) => return Ok(None),
            Err(e) => return Err(e),
        };
        let b = match files::read_at(&dir, "identity.json", wire::MAX_ENROLLMENT_BYTES * 3, true) {
            Ok(b) => Zeroizing::new(b),
            Err(Error::MissingKey) => return Ok(None),
            Err(e) => return Err(e),
        };
        let p: PersistedIdentity =
            serde_json::from_value(wire::strict_json(&b, wire::MAX_ENROLLMENT_BYTES * 3)?)
                .map_err(|_| Error::CorruptRecord)?;
        let response = wire::decode_enrollment_response(
            &STANDARD
                .decode(&p.response)
                .map_err(|_| Error::CorruptRecord)?,
        )?;
        match self.validate(&p.pending, &response) {
            Ok(identity) => {
                // Complete a crash between identity rename and pending removal.
                if let Ok(b) =
                    files::read_at(&dir, "pending.json", wire::MAX_ENROLLMENT_BYTES, true)
                {
                    if decode_pending(&b)?.enrollment == identity.enrollment.0 {
                        files::remove(&dir, "pending.json")?;
                    }
                }
                Ok(Some(identity))
            }
            Err(Error::Unauthorized) => Ok(None),
            Err(e) => Err(e),
        }
    }
    fn validate(
        &self,
        p: &PendingIdentity,
        r: &EnrollmentResponse,
    ) -> Result<LocalSigningIdentity> {
        wire::encode_enrollment_response(r)?;
        if r.cluster != self.cluster
            || p.cluster != self.cluster.0
            || r.enrollment.0 != p.enrollment
        {
            return Err(Error::Unauthorized);
        }
        let certs: Vec<_> = r
            .certificate_chain
            .iter()
            .cloned()
            .map(rustls::pki_types::CertificateDer::from)
            .collect();
        verifier(&self.roots.borrow())?
            .verify_client_cert(&certs[0], &certs[1..], rustls::pki_types::UnixTime::now())
            .map_err(|_| Error::Unauthorized)?;
        let (_, cert) = x509_parser::parse_x509_certificate(&r.certificate_chain[0])
            .map_err(|_| Error::Unauthorized)?;
        let san = cert
            .subject_alternative_name()
            .map_err(|_| Error::Unauthorized)?
            .ok_or(Error::Unauthorized)?;
        let expected = format!("spiffe://{}/node/{}", self.cluster.0, r.node.0);
        if san.value.general_names.len() != 1
            || !matches!(&san.value.general_names[0], x509_parser::extensions::GeneralName::URI(uri) if *uri == expected)
        {
            return Err(Error::Unauthorized);
        }
        if cert.public_key().algorithm.algorithm.to_id_string() != "1.3.101.112" || cert.is_ca() {
            return Err(Error::Unauthorized);
        }
        let usage = cert
            .key_usage()
            .map_err(|_| Error::Unauthorized)?
            .ok_or(Error::Unauthorized)?;
        let extended = cert
            .extended_key_usage()
            .map_err(|_| Error::Unauthorized)?
            .ok_or(Error::Unauthorized)?;
        if !usage.value.digital_signature() || !extended.value.client_auth {
            return Err(Error::Unauthorized);
        }
        use ed25519_dalek::pkcs8::DecodePrivateKey;
        let private_material = Zeroizing::new(
            STANDARD
                .decode(&p.private_key)
                .map_err(|_| Error::CorruptRecord)?,
        );
        let key = ed25519_dalek::SigningKey::from_pkcs8_der(&private_material)
            .map_err(|_| Error::CorruptRecord)?;
        if cert.public_key().subject_public_key.data.as_ref() != key.verifying_key().as_bytes() {
            return Err(Error::Unauthorized);
        }
        let not_before = u64::try_from(cert.validity().not_before.timestamp())
            .map_err(|_| Error::Unauthorized)?;
        let not_after = u64::try_from(cert.validity().not_after.timestamp())
            .map_err(|_| Error::Unauthorized)?;
        if not_after <= not_before
            || not_after - not_before > wire::CERTIFICATE_LIFETIME.as_secs() + 300
        {
            return Err(Error::Unauthorized);
        }
        Ok(LocalSigningIdentity {
            cluster: self.cluster.clone(),
            node: r.node.clone(),
            enrollment: r.enrollment.clone(),
            private_material: private_material.to_vec(),
            certificate_chain: r.certificate_chain.clone(),
            not_before,
            not_after,
        })
    }
}
fn decode_pending(b: &[u8]) -> Result<PendingIdentity> {
    serde_json::from_value(wire::strict_json(b, wire::MAX_ENROLLMENT_BYTES)?)
        .map_err(|_| Error::CorruptRecord)
}
fn verifier(roots: &[Vec<u8>]) -> Result<Arc<dyn rustls::server::danger::ClientCertVerifier>> {
    let mut store = rustls::RootCertStore::empty();
    for r in roots {
        store
            .add(rustls::pki_types::CertificateDer::from(r.clone()))
            .map_err(|_| Error::Unauthorized)?;
    }
    rustls::server::WebPkiClientVerifier::builder_with_provider(
        Arc::new(store),
        Arc::new(rustls::crypto::ring::default_provider()),
    )
    .build()
    .map_err(|_| Error::Unauthorized)
}
impl LocalSigningIdentity {
    pub fn node(&self) -> &NodeId {
        &self.node
    }
    pub fn cluster(&self) -> &ClusterId {
        &self.cluster
    }
    pub fn certificate_chain(&self) -> &[Vec<u8>] {
        &self.certificate_chain
    }
    pub(crate) fn private_key_der(&self) -> &[u8] {
        &self.private_material
    }
    pub fn signing_identity(
        &self,
        roots: &[Vec<u8>],
    ) -> Result<Arc<crate::security::identity::SigningIdentity>> {
        crate::security::identity::SigningIdentity::from_pkcs8(
            self.cluster.clone(),
            self.node.clone(),
            &self.private_material,
            self.certificate_chain.clone(),
            roots,
        )
        .map(Arc::new)
    }
    pub fn expires_at(&self) -> SystemTime {
        UNIX_EPOCH + Duration::from_secs(self.not_after)
    }
    pub fn valid_now(&self) -> bool {
        SystemTime::now() >= UNIX_EPOCH + Duration::from_secs(self.not_before)
            && SystemTime::now() < self.expires_at()
    }
    pub fn renewal_due(&self) -> bool {
        SystemTime::now() >= UNIX_EPOCH + Duration::from_secs(self.not_before) + wire::RENEW_AFTER
    }
}
#[cfg(test)]
mod tests {
    use super::*;
    use crate::control::testing;
    #[test]
    fn durable_retry_key_pairing_identity_and_rotation() {
        let directory = testing::Directory::new();
        let token = directory.0.join("token");
        std::fs::write(&token, "first.token").unwrap();
        let cluster = ClusterId("11111111-1111-4111-8111-111111111111".into());
        let enrollment =
            Enrollment::new(cluster.clone(), token.clone(), directory.0.join("identity"));
        assert!(!directory.0.join("identity").exists());
        let request = enrollment.prepare_now().unwrap();
        let again = Enrollment::new(cluster, token.clone(), directory.0.join("identity"));
        assert_eq!(request.csr_der, again.prepare_now().unwrap().csr_der);
        assert_eq!(request.enrollment, again.prepare_now().unwrap().enrollment);
        assert_eq!(&*again.read_token().unwrap(), "first.token");
        std::fs::write(&token, "rotated.token").unwrap();
        assert_eq!(&*again.read_token().unwrap(), "rotated.token");
        let (ca, key) = testing::ca();
        again.set_peer_trust_roots(vec![ca.der().to_vec()]).unwrap();
        let response = testing::issue(&request, &ca, &key, "22222222-2222-4222-8222-222222222222");
        let mut wrong = response.clone();
        wrong.node.0 = "33333333-3333-4333-8333-333333333333".into();
        assert!(again.accept_response(wrong).is_err());
        assert!(directory.0.join("identity/pending.json").exists());
        let identity = again.accept_response(response).unwrap();
        assert!(identity.valid_now());
        assert!(!identity.renewal_due());
        assert_eq!(
            again.load_identity().unwrap().unwrap().node(),
            identity.node()
        );
        assert!(!directory.0.join("identity/pending.json").exists());
        let fresh = again.prepare_now().unwrap();
        assert_ne!(fresh.enrollment, request.enrollment);
        assert_ne!(fresh.csr_der, request.csr_der);
        assert_eq!(
            again.load_identity().unwrap().unwrap().node(),
            identity.node()
        );
        let dir = files::directory(&directory.0.join("identity"), false, true).unwrap();
        files::atomic_write(&dir, "pending.json", b"{broken").unwrap();
        assert!(again.prepare_now().is_err());
    }
    #[test]
    fn rejects_symlinked_identity_and_insecure_modes() {
        use std::os::unix::fs::{PermissionsExt, symlink};
        let directory = testing::Directory::new();
        std::fs::create_dir(directory.0.join("real")).unwrap();
        symlink("real", directory.0.join("link")).unwrap();
        let enrollment = Enrollment::new(
            ClusterId("11111111-1111-4111-8111-111111111111".into()),
            directory.0.join("token"),
            directory.0.join("link"),
        );
        assert!(enrollment.prepare_now().is_err());
        std::fs::set_permissions(
            directory.0.join("real"),
            std::fs::Permissions::from_mode(0o755),
        )
        .unwrap();
        assert!(files::directory(&directory.0.join("real"), false, true).is_err());
    }
}
