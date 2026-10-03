//! Generate node-private Ed25519 keys locally and enroll/rotate with node-bound SA identity.
use super::wire;
use super::wire::{EnrollmentId, EnrollmentRequest, EnrollmentResponse};
use crate::{
    error::{Error, Operation, Result},
    model::{ClusterId, NodeId},
    runtime::deadline::RequestScope,
};
use base64::{Engine, engine::general_purpose::STANDARD};
use serde::{Deserialize, Serialize};
use std::{
    cell::{Cell, RefCell},
    path::PathBuf,
    rc::Rc,
    sync::Arc,
    time::{Duration, SystemTime, UNIX_EPOCH},
};
use zeroize::{Zeroize, Zeroizing};
pub struct Enrollment {
    inventory: Arc<crate::rdma::discovery::Inventory>,
    inventory_restored: Cell<bool>,
    shares: Cell<u32>,
    cluster: ClusterId,
    token_path: PathBuf,
    identity_directory: PathBuf,
    roots: RefCell<Vec<Vec<u8>>>,
    reactor: RefCell<Option<Rc<crate::runtime::reactor::Reactor>>>,
    busy: Cell<bool>,
    previous: Cell<Option<crate::model::RequestId>>,
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
impl PersistedIdentity {
    fn decode(bytes: &[u8]) -> Result<Self> {
        serde_json::from_value(wire::strict_json(bytes, wire::MAX_ENROLLMENT_BYTES * 3)?)
            .map_err(|_| Error::CorruptRecord)
    }

    fn response(&self) -> Result<EnrollmentResponse> {
        wire::decode_enrollment_response(
            &STANDARD
                .decode(&self.response)
                .map_err(|_| Error::CorruptRecord)?,
        )
    }

    fn encode(&self) -> Result<Zeroizing<Vec<u8>>> {
        Ok(Zeroizing::new(
            serde_json::to_vec(self).map_err(|_| Error::Io)?,
        ))
    }
}
struct TransactionGuard<'a>(&'a Cell<bool>);
impl Drop for TransactionGuard<'_> {
    fn drop(&mut self) {
        self.0.set(false);
    }
}
fn token(b: &[u8]) -> Result<Zeroizing<String>> {
    let token = Zeroizing::new(
        std::str::from_utf8(b)
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
impl Enrollment {
    pub fn new(cluster: ClusterId, token_path: PathBuf, identity_directory: PathBuf) -> Self {
        Self {
            inventory: crate::rdma::discovery::Inventory::shared(),
            inventory_restored: Cell::new(false),
            shares: Cell::new(4),
            cluster,
            token_path,
            identity_directory,
            roots: RefCell::new(Vec::new()),
            reactor: RefCell::new(None),
            busy: Cell::new(false),
            previous: Cell::new(None),
        }
    }
    pub fn set_shares(&self, shares: std::num::NonZeroU32) {
        self.shares.set(shares.get());
    }
    pub fn with_inventory(mut self, inventory: Arc<crate::rdma::discovery::Inventory>) -> Self {
        self.inventory = inventory;
        self
    }
    /// Persist a fresh private key and retry-stable request before submission.
    /// All issuance uses the projected token, including lifetime-based renewal.
    pub fn prepare<'a>(&'a self, scope: &'a RequestScope) -> Operation<'a, EnrollmentRequest> {
        Box::pin(async move {
            let r = self.reactor()?;
            let (_guard, scope) = self.begin(&r, scope).await?;
            let dir =
                super::async_files::directory(&r, &self.identity_directory, true, true, &scope)
                    .await?;
            if !self.inventory_restored.get() {
                match super::async_files::read_at(
                    &r,
                    &dir,
                    "rdma-rails.json",
                    crate::rdma::discovery::MAX_JOURNAL_BYTES,
                    true,
                    &scope,
                )
                .await
                {
                    Ok(bytes) => self.inventory.restore(&bytes)?,
                    Err(Error::MissingKey) => (),
                    Err(error) => return Err(error),
                }
                self.inventory_restored.set(true);
            }
            self.inventory.refresh()?;
            // Persist reservations before reporting. A crash can withdraw ports,
            // but must never reuse an old physical port's automatic rail.
            let reservations = self.inventory.reservations()?;
            if reservations.len() > crate::rdma::discovery::MAX_JOURNAL_BYTES {
                return Err(Error::Overloaded);
            }
            super::async_files::atomic_write(&r, &dir, "rdma-rails.json", &reservations, &scope)
                .await?;
            match super::async_files::read_at(
                &r,
                &dir,
                "pending.json",
                wire::MAX_ENROLLMENT_BYTES,
                true,
                &scope,
            )
            .await
            {
                Ok(b) => {
                    r.file_sync(dir.clone(), &scope).await?;
                    return self.request(&decode_pending(&b)?);
                }
                Err(Error::MissingKey) => (),
                Err(e) => return Err(e),
            }
            let pending = self.generate()?;
            let encoded = Zeroizing::new(serde_json::to_vec(&pending).map_err(|_| Error::Io)?);
            super::async_files::atomic_write(&r, &dir, "pending.json", &encoded, &scope).await?;
            self.request(&pending)
        })
    }
    pub fn attach_reactor(&self, reactor: Rc<crate::runtime::reactor::Reactor>) {
        *self.reactor.borrow_mut() = Some(reactor);
    }
    fn reactor(&self) -> Result<Rc<crate::runtime::reactor::Reactor>> {
        self.reactor
            .borrow()
            .clone()
            .ok_or(Error::InvalidConfiguration)
    }
    async fn begin<'a>(
        &'a self,
        r: &crate::runtime::reactor::Reactor,
        scope: &RequestScope,
    ) -> Result<(TransactionGuard<'a>, RequestScope)> {
        scope.check()?;
        if self.busy.replace(true) {
            return Err(Error::Overloaded);
        }
        let guard = TransactionGuard(&self.busy);
        if let Some(id) = self.previous.get() {
            r.file_fence(id).await?;
        }
        let mut scope = scope.clone();
        let mut id = [0; 16];
        uring_runtime::environment::fill_random(&mut id).map_err(|_| Error::Io)?;
        scope.request = crate::model::RequestId(id);
        self.previous.set(Some(scope.request));
        Ok((guard, scope))
    }
    pub fn read_token_async<'a>(
        &'a self,
        scope: &'a RequestScope,
    ) -> Operation<'a, Zeroizing<String>> {
        Box::pin(async move {
            let r = self.reactor()?;
            let b = super::async_files::read_path(
                &r,
                &self.token_path,
                wire::MAX_ENROLLMENT_BYTES,
                scope,
            )
            .await?;
            token(&b)
        })
    }
    pub fn cluster(&self) -> &ClusterId {
        &self.cluster
    }
    pub fn accept_response_async<'a>(
        &'a self,
        response: EnrollmentResponse,
        scope: &'a RequestScope,
    ) -> Operation<'a, LocalSigningIdentity> {
        Box::pin(async move {
            use super::async_files as af;
            let r = self.reactor()?;
            let (_guard, scope) = self.begin(&r, scope).await?;
            let dir = af::directory(&r, &self.identity_directory, false, true, &scope).await?;
            match af::read_at(
                &r,
                &dir,
                "identity.json",
                wire::MAX_ENROLLMENT_BYTES * 3,
                true,
                &scope,
            )
            .await
            {
                Ok(b) => {
                    let old = PersistedIdentity::decode(&b)?.response()?;
                    if old.cluster != response.cluster {
                        return Err(Error::Unauthorized);
                    }
                }
                Err(Error::MissingKey) => (),
                Err(e) => return Err(e),
            }
            let bytes = af::read_at(
                &r,
                &dir,
                "pending.json",
                wire::MAX_ENROLLMENT_BYTES,
                true,
                &scope,
            )
            .await?;
            let (identity, bytes) = self.accept_pending(&bytes, &response)?;
            af::atomic_write(&r, &dir, "identity.json", &bytes, &scope).await?;
            af::remove(&r, &dir, "pending.json", &scope).await?;
            Ok(identity)
        })
    }
    pub fn load_identity_async<'a>(
        &'a self,
        scope: &'a RequestScope,
    ) -> Operation<'a, Option<LocalSigningIdentity>> {
        Box::pin(async move {
            use super::async_files as af;
            let r = self.reactor()?;
            let (_guard, scope) = self.begin(&r, scope).await?;
            let dir = match af::directory(&r, &self.identity_directory, false, true, &scope).await {
                Ok(dir) => dir,
                Err(Error::MissingKey) => return Ok(None),
                Err(e) => return Err(e),
            };
            let bytes = match af::read_at(
                &r,
                &dir,
                "identity.json",
                wire::MAX_ENROLLMENT_BYTES * 3,
                true,
                &scope,
            )
            .await
            {
                Ok(b) => b,
                Err(Error::MissingKey) => return Ok(None),
                Err(e) => return Err(e),
            };
            let p = PersistedIdentity::decode(&bytes)?;
            let response = p.response()?;
            match self.validate(&p.pending, &response) {
                Ok(identity) => {
                    r.file_sync(dir.clone(), &scope).await?;
                    match af::read_at(
                        &r,
                        &dir,
                        "pending.json",
                        wire::MAX_ENROLLMENT_BYTES,
                        true,
                        &scope,
                    )
                    .await
                    {
                        Ok(b) if decode_pending(&b)?.enrollment == identity.enrollment.0 => {
                            af::remove(&r, &dir, "pending.json", &scope).await?
                        }
                        Ok(_) | Err(Error::MissingKey) => (),
                        Err(e) => return Err(e),
                    }
                    Ok(Some(identity))
                }
                Err(Error::Unauthorized) => Ok(None),
                Err(e) => Err(e),
            }
        })
    }
    pub fn set_peer_trust_roots(&self, roots: Vec<Vec<u8>>) -> Result<()> {
        verifier(&roots)?;
        *self.roots.borrow_mut() = roots;
        Ok(())
    }
    fn generate(&self) -> Result<PendingIdentity> {
        if !wire::valid_uuid(&self.cluster.0) {
            return Err(Error::InvalidConfiguration);
        }
        let key = rcgen::KeyPair::generate_for(&rcgen::PKCS_ED25519).map_err(|_| Error::Io)?;
        let mut params = rcgen::CertificateParams::new(Vec::<String>::new())
            .map_err(|_| Error::InvalidRequest)?;
        params.distinguished_name = rcgen::DistinguishedName::new();
        let csr = params.serialize_request(&key).map_err(|_| Error::Io)?;
        let mut id = [0; 16];
        uring_runtime::environment::fill_random(&mut id).map_err(|_| Error::Io)?;
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
        Ok(pending)
    }
    fn request(&self, p: &PendingIdentity) -> Result<EnrollmentRequest> {
        if p.cluster != self.cluster.0 {
            return Err(Error::Unauthorized);
        }
        let r = EnrollmentRequest {
            rdma_nics: self.inventory.snapshot()?.nics,
            shares: self.shares.get(),
            schema_version: 1,
            cluster: self.cluster.clone(),
            enrollment: EnrollmentId(p.enrollment.clone()),
            csr_der: STANDARD.decode(&p.csr).map_err(|_| Error::CorruptRecord)?,
        };
        wire::encode_enrollment_request(&r)?;
        // A corrupt pending key must never be submitted, even if its CSR parses.
        use x509_parser::prelude::FromDer;
        let secret = Zeroizing::new(
            STANDARD
                .decode(&p.private_key)
                .map_err(|_| Error::CorruptRecord)?,
        );
        let key = racer_crypto::ed25519::SigningKey::from_pkcs8_der(&secret)
            .map_err(|_| Error::CorruptRecord)?;
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
    /// Validate identity correlation and key pairing before fenced persistence.
    fn accept_pending(
        &self,
        bytes: &[u8],
        response: &EnrollmentResponse,
    ) -> Result<(LocalSigningIdentity, Zeroizing<Vec<u8>>)> {
        let pending = decode_pending(bytes)?;
        self.request(&pending)?;
        let identity = self.validate(&pending, response)?;
        let persisted = PersistedIdentity {
            pending,
            response: STANDARD.encode(wire::encode_enrollment_response(response)?),
        };
        Ok((identity, persisted.encode()?))
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
            .verify_client_cert(&certs[0], &certs[1..], crate::runtime::unix_time())
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
        let private_material = Zeroizing::new(
            STANDARD
                .decode(&p.private_key)
                .map_err(|_| Error::CorruptRecord)?,
        );
        let key = racer_crypto::ed25519::SigningKey::from_pkcs8_der(&private_material)
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
    ) -> Result<Arc<racer_identity::SigningIdentity>> {
        racer_identity::SigningIdentity::from_pkcs8(
            self.cluster.clone(),
            self.node.clone(),
            &self.private_material,
            self.certificate_chain.clone(),
            roots,
        )
        .map(Arc::new)
        .map_err(Into::into)
    }
    pub fn expires_at(&self) -> SystemTime {
        UNIX_EPOCH + Duration::from_secs(self.not_after)
    }
    pub fn valid_now(&self) -> bool {
        uring_runtime::environment::wall_now() >= UNIX_EPOCH + Duration::from_secs(self.not_before)
            && uring_runtime::environment::wall_now() < self.expires_at()
    }
    pub fn renewal_due(&self) -> bool {
        let lifetime = Duration::from_secs(self.not_after - self.not_before);
        uring_runtime::environment::wall_now()
            >= UNIX_EPOCH
                + Duration::from_secs(self.not_before)
                + (lifetime * 2 / 3).min(wire::RENEW_AFTER)
    }
}
#[cfg(test)]
mod tests {
    #[test]
    fn enrollment_and_renewal_refresh_authenticated_physical_inventory() {
        use crate::rdma::lifecycle::simulation::{Device, Simulation};
        let enrollment = super::Enrollment::new(
            crate::model::ClusterId("11111111-1111-4111-8111-111111111111".into()),
            "/unused/token".into(),
            "/unused/identity".into(),
        );
        let pending = enrollment.generate().unwrap();
        let first = Simulation::new()
            .with_devices(vec![Device::new("nic-a", [1; 16])])
            .unwrap();
        let request = {
            let _environment = first.enter();
            enrollment.inventory.refresh().unwrap();
            enrollment.request(&pending).unwrap()
        };
        assert_eq!(request.rdma_nics.len(), 1);
        assert_eq!(request.rdma_nics[0].device, "nic-a");
        assert_eq!(request.rdma_nics[0].gid, Some([1; 16]));
        let second = Simulation::new()
            .with_devices(vec![Device::new("nic-b", [2; 16])])
            .unwrap();
        let _environment = second.enter();
        enrollment.inventory.refresh().unwrap();
        let renewed = enrollment.request(&pending).unwrap();
        assert_eq!(renewed.enrollment, request.enrollment);
        assert_eq!(renewed.csr_der, request.csr_der);
        assert_eq!(renewed.rdma_nics[0].device, "nic-b");
        let absent = Simulation::new().with_devices(vec![]).unwrap();
        let _environment = absent.enter();
        enrollment.inventory.refresh().unwrap();
        assert!(enrollment.request(&pending).unwrap().rdma_nics.is_empty());
    }
    use super::*;
    use crate::control::testing;
    const OLD_NODE: &str = "22222222-2222-4222-8222-222222222222";
    const NEW_NODE: &str = "33333333-3333-4333-8333-333333333333";

    #[test]
    fn renewal_tracks_short_issued_lifetime_and_preserves_default() {
        for (lifetime, due) in [(120, 80), (86400, 57600), (86700, 57600)] {
            let identity = LocalSigningIdentity {
                cluster: ClusterId(String::new()),
                node: NodeId(String::new()),
                enrollment: EnrollmentId(String::new()),
                private_material: Vec::new(),
                certificate_chain: Vec::new(),
                not_before: 1000,
                not_after: 1000 + lifetime,
            };
            let clock = uring_runtime::environment::SimulationClock::new_at(
                51,
                std::time::Instant::now(),
                UNIX_EPOCH + Duration::from_secs(1000 + due - 1),
            );
            let _time = clock.environment(1).enter();
            assert!(!identity.renewal_due());
            clock.advance(Duration::from_secs(1));
            assert!(identity.renewal_due());
            assert!(identity.valid_now());
            clock.advance(Duration::from_secs(lifetime - due));
            assert!(!identity.valid_now());
            assert!(identity.renewal_due());
        }
    }

    #[test]
    fn authenticated_replacement_preserves_cluster_key_and_correlation_checks() {
        let Some(r) = testing::reactor() else { return };
        for expired in [false, true] {
            let d = testing::Directory::new();
            let e = Enrollment::new(
                ClusterId("11111111-1111-4111-8111-111111111111".into()),
                d.0.join("token"),
                d.0.join("identity"),
            );
            e.attach_reactor(r.clone());
            let (ca, key) = testing::ca();
            e.set_peer_trust_roots(vec![ca.der().to_vec()]).unwrap();
            let scope = testing::scope();
            let request = testing::drive(&r, e.prepare(&scope)).unwrap();
            testing::drive(
                &r,
                e.accept_response_async(testing::issue(&request, &ca, &key, OLD_NODE), &scope),
            )
            .unwrap();
            let clock = uring_runtime::environment::SimulationClock::new_at(
                31,
                std::time::Instant::now(),
                SystemTime::now() + Duration::from_secs(172800),
            );
            let _time = expired.then(|| clock.environment(1).enter());
            if expired {
                assert!(
                    testing::drive(&r, e.load_identity_async(&scope))
                        .unwrap()
                        .is_none()
                );
            }
            let old = std::fs::read(d.0.join("identity/identity.json")).unwrap();
            let request = testing::drive(&r, e.prepare(&scope)).unwrap();
            let response = testing::issue(&request, &ca, &key, NEW_NODE);
            let mut wrong_san = response.clone();
            wrong_san.node = NodeId(OLD_NODE.into());
            let mut wrong_id = response.clone();
            wrong_id.enrollment = EnrollmentId(OLD_NODE.into());
            let mut foreign_request = request.clone();
            foreign_request.cluster = ClusterId("44444444-4444-4444-8444-444444444444".into());
            let foreign = testing::issue(&foreign_request, &ca, &key, NEW_NODE);
            let (rogue_ca, rogue_key) = testing::ca();
            let rogue = testing::issue(&request, &rogue_ca, &rogue_key, NEW_NODE);
            let other = Enrollment::new(e.cluster.clone(), d.0.join("token"), d.0.join("other"));
            other.attach_reactor(r.clone());
            let mut other_request = testing::drive(&r, other.prepare(&scope)).unwrap();
            other_request.enrollment = request.enrollment.clone();
            let wrong_key = testing::issue(&other_request, &ca, &key, NEW_NODE);
            for bad in [wrong_san, wrong_id, foreign, rogue, wrong_key] {
                assert!(matches!(
                    testing::drive(&r, e.accept_response_async(bad, &scope)),
                    Err(Error::Unauthorized)
                ));
                assert_eq!(
                    std::fs::read(d.0.join("identity/identity.json")).unwrap(),
                    old
                );
                assert!(d.0.join("identity/pending.json").exists());
            }
            let identity = testing::drive(&r, e.accept_response_async(response, &scope)).unwrap();
            assert_eq!(identity.node().0, NEW_NODE);
            assert_eq!(
                testing::drive(&r, e.load_identity_async(&scope))
                    .unwrap()
                    .unwrap()
                    .node()
                    .0,
                NEW_NODE
            );
            assert!(!d.0.join("identity/pending.json").exists());

            // Even with new configuration, roots, and a fresh pending request,
            // a hostPath pinned by an existing identity cannot adopt a cluster.
            let foreign = Enrollment::new(
                foreign_request.cluster,
                d.0.join("token"),
                d.0.join("identity"),
            );
            foreign
                .set_peer_trust_roots(vec![ca.der().to_vec()])
                .unwrap();
            foreign.attach_reactor(r.clone());
            let request = testing::drive(&r, foreign.prepare(&scope)).unwrap();
            assert!(matches!(
                testing::drive(
                    &r,
                    foreign.accept_response_async(
                        testing::issue(&request, &ca, &key, NEW_NODE),
                        &scope
                    )
                ),
                Err(Error::Unauthorized)
            ));
        }
    }

    #[test]
    fn replacement_crash_at_each_completion_recovers_and_reauthenticates() {
        let Some(r) = testing::reactor() else { return };
        let (ca, key) = testing::ca();
        let mut saw_old = false;
        let mut saw_new = false;
        let mut saw_committed_pending = false;
        for boundary in 0..45 {
            let d = testing::Directory::new();
            let enrollment = || {
                let e = Enrollment::new(
                    ClusterId("11111111-1111-4111-8111-111111111111".into()),
                    d.0.join("token"),
                    d.0.join("identity"),
                );
                e.attach_reactor(r.clone());
                e.set_peer_trust_roots(vec![ca.der().to_vec()]).unwrap();
                e
            };
            let e = enrollment();
            let scope = testing::scope();
            let request = testing::drive(&r, e.prepare(&scope)).unwrap();
            testing::drive(
                &r,
                e.accept_response_async(testing::issue(&request, &ca, &key, OLD_NODE), &scope),
            )
            .unwrap();
            let request = testing::drive(&r, e.prepare(&scope)).unwrap();
            let mut accept =
                e.accept_response_async(testing::issue(&request, &ca, &key, NEW_NODE), &scope);
            let mut cx = std::task::Context::from_waker(futures::task::noop_waker_ref());
            for _ in 0..boundary {
                if accept.as_mut().poll(&mut cx).is_ready() {
                    break;
                }
                let until = std::time::Instant::now() + Duration::from_secs(5);
                while r.in_flight() != 0 {
                    assert!(std::time::Instant::now() < until);
                    if r.poll_budgeted(1).unwrap() == 0 {
                        r.wait(Duration::from_millis(1)).unwrap();
                    }
                }
            }
            drop(accept);
            testing::drive(&r, r.file_fence(e.previous.get().unwrap())).unwrap();
            let pending_exists = d.0.join("identity/pending.json").exists();
            let restarted = enrollment();
            let recovered = testing::drive(&r, restarted.load_identity_async(&scope))
                .unwrap()
                .unwrap();
            saw_old |= recovered.node().0 == OLD_NODE;
            saw_new |= recovered.node().0 == NEW_NODE;
            saw_committed_pending |= recovered.node().0 == NEW_NODE && pending_exists;
            assert!([OLD_NODE, NEW_NODE].contains(&recovered.node().0.as_str()));
            // Startup must submit a request again, regardless of what survived.
            let request = testing::drive(&r, restarted.prepare(&scope)).unwrap();
            let current = testing::drive(
                &r,
                restarted
                    .accept_response_async(testing::issue(&request, &ca, &key, NEW_NODE), &scope),
            )
            .unwrap();
            assert_eq!(current.node().0, NEW_NODE);
            assert!(!d.0.join("identity/pending.json").exists());
        }
        assert!(saw_old && saw_new && saw_committed_pending);
        assert_eq!(r.in_flight(), 0);
    }

    #[test]
    fn durable_retry_key_pairing_identity_and_rotation() {
        let Some(r) = testing::reactor() else { return };
        let scope = testing::scope();
        let directory = testing::Directory::new();
        let token = directory.0.join("token");
        std::fs::write(&token, "first.token").unwrap();
        let cluster = ClusterId("11111111-1111-4111-8111-111111111111".into());
        let enrollment =
            Enrollment::new(cluster.clone(), token.clone(), directory.0.join("identity"));
        assert!(!directory.0.join("identity").exists());
        enrollment.attach_reactor(r.clone());
        let request = testing::drive(&r, enrollment.prepare(&scope)).unwrap();
        let again = Enrollment::new(cluster, token.clone(), directory.0.join("identity"));
        again.attach_reactor(r.clone());
        assert_eq!(
            request.csr_der,
            testing::drive(&r, again.prepare(&scope)).unwrap().csr_der
        );
        assert_eq!(
            request.enrollment,
            testing::drive(&r, again.prepare(&scope))
                .unwrap()
                .enrollment
        );
        assert_eq!(
            &*testing::drive(&r, again.read_token_async(&scope)).unwrap(),
            "first.token"
        );
        std::fs::write(&token, "rotated.token").unwrap();
        assert_eq!(
            &*testing::drive(&r, again.read_token_async(&scope)).unwrap(),
            "rotated.token"
        );
        let (ca, key) = testing::ca();
        again.set_peer_trust_roots(vec![ca.der().to_vec()]).unwrap();
        let response = testing::issue(&request, &ca, &key, "22222222-2222-4222-8222-222222222222");
        let mut wrong = response.clone();
        wrong.node.0 = "33333333-3333-4333-8333-333333333333".into();
        assert!(testing::drive(&r, again.accept_response_async(wrong, &scope)).is_err());
        assert!(directory.0.join("identity/pending.json").exists());
        let identity = testing::drive(&r, again.accept_response_async(response, &scope)).unwrap();
        assert!(identity.valid_now());
        assert!(!identity.renewal_due());
        assert_eq!(
            testing::drive(&r, again.load_identity_async(&scope))
                .unwrap()
                .unwrap()
                .node(),
            identity.node()
        );
        assert!(!directory.0.join("identity/pending.json").exists());
        let fresh = testing::drive(&r, again.prepare(&scope)).unwrap();
        assert_ne!(fresh.enrollment, request.enrollment);
        assert_ne!(fresh.csr_der, request.csr_der);
        assert_eq!(
            testing::drive(&r, again.load_identity_async(&scope))
                .unwrap()
                .unwrap()
                .node(),
            identity.node()
        );
        std::fs::write(directory.0.join("identity/pending.json"), b"{broken").unwrap();
        assert!(testing::drive(&r, again.prepare(&scope)).is_err());
    }
    #[test]
    fn durable_rail_journal_prevents_restart_renumbering_and_corruption_fails_closed() {
        use crate::rdma::lifecycle::simulation::{Device, Simulation};
        let Some(r) = testing::reactor() else { return };
        let scope = testing::scope();
        let directory = testing::Directory::new();
        let make = || {
            let e = Enrollment::new(
                ClusterId("11111111-1111-4111-8111-111111111111".into()),
                directory.0.join("token"),
                directory.0.join("identity"),
            );
            e.attach_reactor(r.clone());
            e
        };
        let all = Simulation::new()
            .with_devices(vec![Device::new("a", [1; 16]), Device::new("b", [2; 16])])
            .unwrap();
        let first = {
            let _environment = all.enter();
            testing::drive(&r, make().prepare(&scope)).unwrap()
        };
        assert_eq!(first.rdma_nics[1].rail.0, 1);
        let only_b = Simulation::new()
            .with_devices(vec![Device::new("b", [3; 16])])
            .unwrap();
        let _environment = only_b.enter();
        let restarted = testing::drive(&r, make().prepare(&scope)).unwrap();
        assert_eq!(restarted.rdma_nics.len(), 1);
        assert_eq!(restarted.rdma_nics[0].rail.0, 1);
        assert_eq!(restarted.rdma_nics[0].gid, Some([3; 16]));
        std::fs::write(directory.0.join("identity/rdma-rails.json"), b"broken").unwrap();
        assert!(matches!(
            testing::drive(&r, make().prepare(&scope)),
            Err(Error::CorruptRecord)
        ));
    }
    #[test]
    fn saturated_rail_journal_still_persists_and_enrolls_known_ports() {
        use crate::{
            rdma::{
                discovery::{Inventory, MAX_JOURNAL_BYTES},
                lifecycle::simulation::{Device, Simulation},
            },
            topology::rails::{RailId, RailMapping},
        };
        let Some(r) = testing::reactor() else { return };
        let scope = testing::scope();
        let directory = testing::Directory::new();
        let inventory = Inventory::shared();
        let name = |i| format!("{i:04}{}", "x".repeat(59));
        for batch in 0..20 {
            inventory
                .update(
                    (batch * 64..(batch + 1) * 64)
                        .map(|i| RailMapping {
                            device: name(i),
                            port: 1,
                            rail: RailId(0),
                            gid: Some([1; 16]),
                            numa_node: None,
                        })
                        .collect(),
                )
                .unwrap();
            assert!(inventory.reservations().unwrap().len() <= MAX_JOURNAL_BYTES);
        }
        let journal = inventory.reservations().unwrap();
        let sim = Simulation::new()
            .with_devices(vec![
                Device::new(name(0), [2; 16]),
                Device::new(name(2000), [3; 16]),
            ])
            .unwrap();
        let _environment = sim.enter();
        let make = || {
            let e = Enrollment::new(
                ClusterId("11111111-1111-4111-8111-111111111111".into()),
                directory.0.join("token"),
                directory.0.join("identity"),
            )
            .with_inventory(inventory.clone());
            e.attach_reactor(r.clone());
            e
        };
        for _ in 0..2 {
            let request = testing::drive(&r, make().prepare(&scope)).unwrap();
            assert_eq!(request.rdma_nics.len(), 1);
            assert_eq!(request.rdma_nics[0].device, name(0));
            assert_eq!(request.rdma_nics[0].rail, RailId(0));
            assert_eq!(request.rdma_nics[0].gid, Some([2; 16]));
            assert_eq!(
                std::fs::read(directory.0.join("identity/rdma-rails.json")).unwrap(),
                journal
            );
        }
    }
    #[test]
    fn rejects_symlinked_identity_and_insecure_modes() {
        let Some(r) = testing::reactor() else { return };
        let scope = testing::scope();
        use std::os::unix::fs::{PermissionsExt, symlink};
        let directory = testing::Directory::new();
        std::fs::create_dir(directory.0.join("real")).unwrap();
        symlink("real", directory.0.join("link")).unwrap();
        let enrollment = Enrollment::new(
            ClusterId("11111111-1111-4111-8111-111111111111".into()),
            directory.0.join("token"),
            directory.0.join("link"),
        );
        enrollment.attach_reactor(r.clone());
        assert!(testing::drive(&r, enrollment.prepare(&scope)).is_err());
        std::fs::set_permissions(
            directory.0.join("real"),
            std::fs::Permissions::from_mode(0o755),
        )
        .unwrap();
        let insecure = Enrollment::new(
            enrollment.cluster.clone(),
            directory.0.join("token"),
            directory.0.join("real"),
        );
        insecure.attach_reactor(r.clone());
        assert!(testing::drive(&r, insecure.prepare(&scope)).is_err());
    }
}
