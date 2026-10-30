//! Certificates: seat issuer roots, node certificates, client certificate
//! checks, and serving identities.
//!
//! Node certificates match the old controller: an Ed25519 leaf with only
//! digitalSignature, only clientAuth, and one SAN
//! `spiffe://<cluster>/node/<node uid>`, issued for the CSR key and chained to
//! the seat root. Issuer private keys live only in memory.

use racer_control_wire as wire;
use rcgen::{
    BasicConstraints, CertificateParams, DnType, ExtendedKeyUsagePurpose, IsCa, KeyPair,
    KeyUsagePurpose, SanType, SerialNumber, SubjectPublicKeyInfo,
};
use rustls::{
    DigitallySignedStruct, DistinguishedName, SignatureScheme,
    client::danger::HandshakeSignatureValid,
    pki_types::{CertificateDer, PrivateKeyDer, UnixTime},
    server::danger::{ClientCertVerified, ClientCertVerifier},
};
use std::{
    net::IpAddr,
    sync::{Arc, RwLock},
    time::{Duration, SystemTime, UNIX_EPOCH},
};
use x509_parser::{extensions::GeneralName, prelude::FromDer};

/// Backdate certificates for clock skew.
pub const SKEW: Duration = Duration::from_secs(60);

/// Default seat root lifetime.
pub const ROOT_LIFETIME: Duration = Duration::from_secs(7 * 24 * 3600);

/// Default serving certificate lifetime.
pub const SERVING_LIFETIME: Duration = Duration::from_secs(7 * 24 * 3600);

/// Certificate failures.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum PkiError {
    InvalidRequest,
    Unauthenticated,
    Forbidden,
    Unavailable,
}

fn unix(t: SystemTime) -> u64 {
    t.duration_since(UNIX_EPOCH).unwrap_or_default().as_secs()
}

fn serial() -> Result<SerialNumber, PkiError> {
    let mut b = [0u8; 16];
    getrandom::getrandom(&mut b).map_err(|_| PkiError::Unavailable)?;
    b[0] &= 0x7f;
    b[0] |= 0x01;
    Ok(SerialNumber::from_slice(&b))
}

/// In-memory issuer for one seat.
pub struct SeatIssuer {
    key: KeyPair,
    cert: rcgen::Certificate,
    not_after: SystemTime,
}

impl SeatIssuer {
    /// Generate a new root valid from `now - SKEW` for `lifetime`.
    pub fn generate(cluster: &str, now: SystemTime, lifetime: Duration) -> Result<Self, PkiError> {
        let key = KeyPair::generate_for(&rcgen::PKCS_ED25519).map_err(|_| PkiError::Unavailable)?;
        let mut p =
            CertificateParams::new(Vec::<String>::new()).map_err(|_| PkiError::Unavailable)?;
        p.distinguished_name
            .push(DnType::CommonName, format!("Racer {cluster}"));
        p.is_ca = IsCa::Ca(BasicConstraints::Constrained(0));
        p.key_usages = vec![KeyUsagePurpose::KeyCertSign, KeyUsagePurpose::CrlSign];
        p.serial_number = Some(serial()?);
        p.not_before = (now - SKEW).into();
        let not_after = now + lifetime;
        // Whole seconds, so stored NotAfter matches the certificate.
        let not_after = UNIX_EPOCH + Duration::from_secs(unix(not_after));
        p.not_after = not_after.into();
        let cert = p.self_signed(&key).map_err(|_| PkiError::Unavailable)?;
        Ok(Self {
            key,
            cert,
            not_after,
        })
    }

    pub fn der(&self) -> &[u8] {
        self.cert.der()
    }

    pub fn not_after(&self) -> SystemTime {
        self.not_after
    }

    /// Only issue while every new certificate gets its full lifetime.
    pub fn can_issue(&self, now: SystemTime) -> bool {
        self.not_after >= now + wire::CERTIFICATE_LIFETIME + SKEW
    }

    /// Replace when fewer than two certificate lifetimes remain.
    pub fn should_rotate(&self, now: SystemTime) -> bool {
        self.not_after < now + 2 * wire::CERTIFICATE_LIFETIME
    }

    /// Issue a node certificate for the key in `csr_der`. CSR names are ignored.
    pub fn issue(
        &self,
        cluster: &str,
        node: &str,
        csr_der: &[u8],
        now: SystemTime,
    ) -> Result<Vec<Vec<u8>>, PkiError> {
        if !wire::valid_uuid(cluster) || !wire::valid_uuid(node) {
            return Err(PkiError::Forbidden);
        }
        if !self.can_issue(now) {
            return Err(PkiError::Unavailable);
        }
        let spki = csr_public_key(csr_der)?;
        let public = SubjectPublicKeyInfo::from_der(&spki).map_err(|_| PkiError::InvalidRequest)?;
        let mut p =
            CertificateParams::new(Vec::<String>::new()).map_err(|_| PkiError::Unavailable)?;
        p.serial_number = Some(serial()?);
        p.not_before = (now - SKEW).into();
        p.not_after = (now + wire::CERTIFICATE_LIFETIME)
            .min(self.not_after)
            .into();
        p.is_ca = IsCa::ExplicitNoCa;
        p.key_usages = vec![KeyUsagePurpose::DigitalSignature];
        p.extended_key_usages = vec![ExtendedKeyUsagePurpose::ClientAuth];
        let uri = format!("spiffe://{cluster}/node/{node}");
        p.subject_alt_names = vec![SanType::URI(
            uri.try_into().map_err(|_| PkiError::InvalidRequest)?,
        )];
        p.use_authority_key_identifier_extension = true;
        let leaf = p
            .signed_by(&public, &self.cert, &self.key)
            .map_err(|_| PkiError::Unavailable)?;
        Ok(vec![leaf.der().to_vec(), self.der().to_vec()])
    }
}

/// Verify CSR proof of possession and return its Ed25519 SubjectPublicKeyInfo DER.
pub fn csr_public_key(csr_der: &[u8]) -> Result<Vec<u8>, PkiError> {
    let (rest, csr) =
        x509_parser::certification_request::X509CertificationRequest::from_der(csr_der)
            .map_err(|_| PkiError::InvalidRequest)?;
    if !rest.is_empty() || csr.verify_signature().is_err() {
        return Err(PkiError::InvalidRequest);
    }
    let spki = &csr.certification_request_info.subject_pki;
    if spki.algorithm.algorithm.to_id_string() != "1.3.101.112"
        || spki.algorithm.parameters.is_some()
        || spki.subject_public_key.data.len() != 32
    {
        return Err(PkiError::InvalidRequest);
    }
    Ok(spki.raw.to_vec())
}

/// Authenticated node.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct NodeIdentity {
    pub node: String,
    /// Earliest NotAfter in the verified chain, Unix seconds.
    pub expires: u64,
}

fn webpki_verifier(roots: &[Vec<u8>]) -> Result<Arc<dyn ClientCertVerifier>, PkiError> {
    let mut store = rustls::RootCertStore::empty();
    for r in roots {
        store
            .add(CertificateDer::from(r.clone()))
            .map_err(|_| PkiError::Unavailable)?;
    }
    rustls::server::WebPkiClientVerifier::builder_with_provider(
        Arc::new(store),
        Arc::new(rustls::crypto::ring::default_provider()),
    )
    .allow_unauthenticated()
    .build()
    .map_err(|_| PkiError::Unavailable)
}

/// Check a client chain against the committed roots, like the old controller.
pub fn authenticate_client(
    chain: &[Vec<u8>],
    roots: &[Vec<u8>],
    cluster: &str,
    now: SystemTime,
) -> Result<NodeIdentity, PkiError> {
    if chain.is_empty() || chain.len() > 8 {
        return Err(PkiError::Unauthenticated);
    }
    if roots.is_empty() {
        return Err(PkiError::Unavailable);
    }
    let leaf_der = CertificateDer::from(chain[0].as_slice());
    let intermediates: Vec<_> = chain[1..]
        .iter()
        .map(|c| CertificateDer::from(c.as_slice()))
        .collect();
    let time = UnixTime::since_unix_epoch(now.duration_since(UNIX_EPOCH).unwrap_or_default());
    webpki_verifier(roots)?
        .verify_client_cert(&leaf_der, &intermediates, time)
        .map_err(|_| PkiError::Unauthenticated)?;
    let (rest, leaf) =
        x509_parser::parse_x509_certificate(&chain[0]).map_err(|_| PkiError::Unauthenticated)?;
    let ku = leaf
        .key_usage()
        .ok()
        .flatten()
        .ok_or(PkiError::Unauthenticated)?;
    let eku = leaf
        .extended_key_usage()
        .ok()
        .flatten()
        .ok_or(PkiError::Unauthenticated)?;
    if !rest.is_empty()
        || leaf.is_ca()
        || leaf.public_key().algorithm.algorithm.to_id_string() != "1.3.101.112"
        || ku.value.flags != 1
        || !eku.value.client_auth
        || eku.value.server_auth
        || eku.value.any
        || !eku.value.other.is_empty()
        || eku.value.code_signing
        || eku.value.email_protection
        || eku.value.time_stamping
        || eku.value.ocsp_signing
    {
        return Err(PkiError::Unauthenticated);
    }
    let san = leaf
        .subject_alternative_name()
        .ok()
        .flatten()
        .ok_or(PkiError::Unauthenticated)?;
    let uris: Vec<&str> = san
        .value
        .general_names
        .iter()
        .filter_map(|n| {
            if let GeneralName::URI(u) = n {
                Some(*u)
            } else {
                None
            }
        })
        .collect();
    let [uri] = uris.as_slice() else {
        return Err(PkiError::Unauthenticated);
    };
    let rest = uri
        .strip_prefix("spiffe://")
        .ok_or(PkiError::Unauthenticated)?;
    let (host, node) = rest.split_once("/node/").ok_or(PkiError::Unauthenticated)?;
    if !wire::valid_uuid(node) || !wire::valid_uuid(host) {
        return Err(PkiError::Unauthenticated);
    }
    if host != cluster {
        return Err(PkiError::Forbidden);
    }
    // Earliest expiry across the chain and the root that anchors it.
    let mut expires = u64::MAX;
    for der in chain {
        let (_, c) =
            x509_parser::parse_x509_certificate(der).map_err(|_| PkiError::Unauthenticated)?;
        expires = expires.min(c.validity().not_after.timestamp().max(0) as u64);
    }
    let issuer = leaf.issuer().as_raw();
    for r in roots {
        if let Ok((_, c)) = x509_parser::parse_x509_certificate(r)
            && c.subject().as_raw() == issuer
        {
            expires = expires.min(c.validity().not_after.timestamp().max(0) as u64);
        }
    }
    if unix(now) >= expires {
        return Err(PkiError::Unauthenticated);
    }
    Ok(NodeIdentity {
        node: node.into(),
        expires,
    })
}

/// Client certificate verifier with swappable roots. Anonymous clients pass
/// the handshake; handlers decide which routes need a certificate.
#[derive(Debug)]
pub struct DynamicClientVerifier {
    inner: RwLock<Option<Arc<dyn ClientCertVerifier>>>,
    algorithms: rustls::crypto::WebPkiSupportedAlgorithms,
}

impl DynamicClientVerifier {
    pub fn new() -> Arc<Self> {
        Arc::new(Self {
            inner: RwLock::new(None),
            algorithms: rustls::crypto::ring::default_provider().signature_verification_algorithms,
        })
    }

    /// Install the committed roots. Empty roots reject every certificate.
    pub fn set_roots(&self, roots: &[Vec<u8>]) {
        let next = if roots.is_empty() {
            None
        } else {
            webpki_verifier(roots).ok()
        };
        *self.inner.write().unwrap_or_else(|e| e.into_inner()) = next;
    }
}

impl ClientCertVerifier for DynamicClientVerifier {
    fn offer_client_auth(&self) -> bool {
        true
    }

    fn client_auth_mandatory(&self) -> bool {
        false
    }

    fn root_hint_subjects(&self) -> &[DistinguishedName] {
        &[]
    }

    fn verify_client_cert(
        &self,
        end_entity: &CertificateDer<'_>,
        intermediates: &[CertificateDer<'_>],
        now: UnixTime,
    ) -> Result<ClientCertVerified, rustls::Error> {
        let inner = self.inner.read().unwrap_or_else(|e| e.into_inner()).clone();
        match inner {
            Some(v) => v.verify_client_cert(end_entity, intermediates, now),
            None => Err(rustls::Error::InvalidCertificate(
                rustls::CertificateError::UnknownIssuer,
            )),
        }
    }

    fn verify_tls12_signature(
        &self,
        message: &[u8],
        cert: &CertificateDer<'_>,
        dss: &DigitallySignedStruct,
    ) -> Result<HandshakeSignatureValid, rustls::Error> {
        rustls::crypto::verify_tls12_signature(message, cert, dss, &self.algorithms)
    }

    fn verify_tls13_signature(
        &self,
        message: &[u8],
        cert: &CertificateDer<'_>,
        dss: &DigitallySignedStruct,
    ) -> Result<HandshakeSignatureValid, rustls::Error> {
        rustls::crypto::verify_tls13_signature(message, cert, dss, &self.algorithms)
    }

    fn supported_verify_schemes(&self) -> Vec<SignatureScheme> {
        self.algorithms.supported_schemes()
    }
}

/// A serving identity.
///
/// The control API uses [`ServingIdentity::pinned`]: one self-signed
/// end-entity certificate, pinned byte for byte by clients through
/// `DiscoveredEndpoint.certificate`. The webhook uses
/// [`ServingIdentity::generate`]: a small CA, sent as the `caBundle`, and a
/// leaf. In both cases `ca_der` is what peers trust.
pub struct ServingIdentity {
    pub ca_der: Vec<u8>,
    pub leaf_der: Vec<u8>,
    key_der: zeroize::Zeroizing<Vec<u8>>,
    pub not_after: SystemTime,
}

impl ServingIdentity {
    /// Generate an ECDSA P-256 CA and a serverAuth leaf with the given SANs.
    #[cfg(test)]
    pub fn generate(
        ips: &[IpAddr],
        dns: &[String],
        now: SystemTime,
        lifetime: Duration,
    ) -> Result<Self, PkiError> {
        let ca_key = KeyPair::generate_for(&rcgen::PKCS_ECDSA_P256_SHA256)
            .map_err(|_| PkiError::Unavailable)?;
        let not_after = UNIX_EPOCH + Duration::from_secs(unix(now + lifetime));
        let mut ca =
            CertificateParams::new(Vec::<String>::new()).map_err(|_| PkiError::Unavailable)?;
        ca.distinguished_name
            .push(DnType::CommonName, "Racer serving CA");
        ca.is_ca = IsCa::Ca(BasicConstraints::Constrained(0));
        ca.key_usages = vec![KeyUsagePurpose::KeyCertSign];
        ca.serial_number = Some(serial()?);
        ca.not_before = (now - SKEW).into();
        ca.not_after = not_after.into();
        let ca_cert = ca.self_signed(&ca_key).map_err(|_| PkiError::Unavailable)?;
        let key = KeyPair::generate_for(&rcgen::PKCS_ECDSA_P256_SHA256)
            .map_err(|_| PkiError::Unavailable)?;
        let mut leaf =
            CertificateParams::new(dns.to_vec()).map_err(|_| PkiError::InvalidRequest)?;
        leaf.subject_alt_names
            .extend(ips.iter().map(|ip| SanType::IpAddress(*ip)));
        leaf.distinguished_name
            .push(DnType::CommonName, "racer-control");
        leaf.is_ca = IsCa::ExplicitNoCa;
        leaf.key_usages = vec![KeyUsagePurpose::DigitalSignature];
        leaf.extended_key_usages = vec![ExtendedKeyUsagePurpose::ServerAuth];
        leaf.serial_number = Some(serial()?);
        leaf.not_before = (now - SKEW).into();
        leaf.not_after = not_after.into();
        leaf.use_authority_key_identifier_extension = true;
        let leaf = leaf
            .signed_by(&key, &ca_cert, &ca_key)
            .map_err(|_| PkiError::Unavailable)?;
        Ok(Self {
            ca_der: ca_cert.der().to_vec(),
            leaf_der: leaf.der().to_vec(),
            key_der: zeroize::Zeroizing::new(key.serialize_der()),
            not_after,
        })
    }

    /// Self-signed serverAuth end-entity certificate with IP SANs only.
    pub fn pinned(
        ips: &[IpAddr],
        dns: &[String],
        now: SystemTime,
        lifetime: Duration,
    ) -> Result<Self, PkiError> {
        let key = KeyPair::generate_for(&rcgen::PKCS_ECDSA_P256_SHA256)
            .map_err(|_| PkiError::Unavailable)?;
        let not_after = UNIX_EPOCH + Duration::from_secs(unix(now + lifetime));
        let mut p = CertificateParams::new(dns.to_vec()).map_err(|_| PkiError::InvalidRequest)?;
        p.subject_alt_names
            .extend(ips.iter().map(|ip| SanType::IpAddress(*ip)));
        p.distinguished_name
            .push(DnType::CommonName, "racer-control");
        p.is_ca = IsCa::ExplicitNoCa;
        p.key_usages = vec![KeyUsagePurpose::DigitalSignature];
        p.extended_key_usages = vec![ExtendedKeyUsagePurpose::ServerAuth];
        p.serial_number = Some(serial()?);
        p.not_before = (now - SKEW).into();
        p.not_after = not_after.into();
        let cert = p.self_signed(&key).map_err(|_| PkiError::Unavailable)?;
        Ok(Self {
            ca_der: cert.der().to_vec(),
            leaf_der: cert.der().to_vec(),
            key_der: zeroize::Zeroizing::new(key.serialize_der()),
            not_after,
        })
    }

    /// TLS server config with an optional client verifier.
    pub fn server_config(
        &self,
        clients: Option<Arc<dyn ClientCertVerifier>>,
    ) -> Result<Arc<rustls::ServerConfig>, PkiError> {
        let builder = rustls::ServerConfig::builder_with_provider(Arc::new(
            rustls::crypto::ring::default_provider(),
        ))
        .with_safe_default_protocol_versions()
        .map_err(|_| PkiError::Unavailable)?;
        let builder = match clients {
            Some(v) => builder.with_client_cert_verifier(v),
            None => builder.with_no_client_auth(),
        };
        let mut config = builder
            .with_single_cert(
                if self.leaf_der == self.ca_der {
                    vec![CertificateDer::from(self.leaf_der.clone())]
                } else {
                    vec![
                        CertificateDer::from(self.leaf_der.clone()),
                        CertificateDer::from(self.ca_der.clone()),
                    ]
                },
                PrivateKeyDer::Pkcs8(self.key_der.to_vec().into()),
            )
            .map_err(|_| PkiError::Unavailable)?;
        config.alpn_protocols = vec![b"http/1.1".to_vec()];
        Ok(Arc::new(config))
    }
}

/// SHA-256 hex of DER.
pub fn fingerprint(der: &[u8]) -> String {
    use sha2::Digest;
    format!("{:x}", sha2::Sha256::digest(der))
}

#[cfg(test)]
pub(crate) mod tests {
    use super::*;
    use std::io::{Read, Write};

    pub const CLUSTER: &str = "11111111-1111-4111-8111-111111111111";
    pub const NODE: &str = "22222222-2222-4222-8222-222222222222";

    /// A node key and CSR.
    pub fn csr() -> (KeyPair, Vec<u8>) {
        let key = KeyPair::generate_for(&rcgen::PKCS_ED25519).unwrap();
        let csr = CertificateParams::new(vec!["ignored.example".into()])
            .unwrap()
            .serialize_request(&key)
            .unwrap();
        (key, csr.der().to_vec())
    }

    #[test]
    fn issued_chain_is_accepted_by_the_dataplane_policy() {
        let now = SystemTime::now();
        let issuer = SeatIssuer::generate(CLUSTER, now, ROOT_LIFETIME).unwrap();
        let (_key, csr) = csr();
        let chain = issuer.issue(CLUSTER, NODE, &csr, now).unwrap();
        let id = authenticate_client(&chain, &[issuer.der().to_vec()], CLUSTER, now).unwrap();
        assert_eq!(id.node, NODE);
        let lifetime = id.expires - unix(now);
        assert!(lifetime <= 86400 && lifetime > 86000);
        // The real dataplane identity check accepts the chain for the CSR key.
        let node_key = KeyPair::generate_for(&rcgen::PKCS_ED25519).unwrap();
        let pending =
            racer_crypto::identity::PendingIdentity::recover(&node_key.serialize_der()).unwrap();
        let real = issuer
            .issue(CLUSTER, NODE, &pending.csr_der().unwrap(), now)
            .unwrap();
        pending
            .accept(
                wire::ClusterId(CLUSTER.into()),
                wire::NodeId(NODE.into()),
                real,
                &[issuer.der().to_vec()],
            )
            .unwrap();
        // Wrong cluster and unknown root.
        assert_eq!(
            authenticate_client(
                &chain,
                &[issuer.der().to_vec()],
                "33333333-3333-4333-8333-333333333333",
                now
            ),
            Err(PkiError::Forbidden)
        );
        let other = SeatIssuer::generate(CLUSTER, now, ROOT_LIFETIME).unwrap();
        assert_eq!(
            authenticate_client(&chain, &[other.der().to_vec()], CLUSTER, now),
            Err(PkiError::Unauthenticated)
        );
        assert_eq!(
            authenticate_client(
                &chain,
                &[issuer.der().to_vec()],
                CLUSTER,
                now + Duration::from_secs(2 * 86400)
            ),
            Err(PkiError::Unauthenticated)
        );
        assert_eq!(
            authenticate_client(&chain, &[], CLUSTER, now),
            Err(PkiError::Unavailable)
        );
    }

    #[test]
    fn issuance_refusals() {
        let now = SystemTime::now();
        let issuer = SeatIssuer::generate(CLUSTER, now, ROOT_LIFETIME).unwrap();
        let (_k, mut bad) = csr();
        let n = bad.len() - 5;
        bad[n] ^= 1;
        assert_eq!(
            issuer.issue(CLUSTER, NODE, &bad, now),
            Err(PkiError::InvalidRequest)
        );
        let ec = KeyPair::generate_for(&rcgen::PKCS_ECDSA_P256_SHA256).unwrap();
        let ec_csr = CertificateParams::new(vec![])
            .unwrap()
            .serialize_request(&ec)
            .unwrap();
        assert_eq!(
            issuer.issue(CLUSTER, NODE, ec_csr.der(), now),
            Err(PkiError::InvalidRequest)
        );
        let (_k, good) = csr();
        assert_eq!(
            issuer.issue(CLUSTER, "node", &good, now),
            Err(PkiError::Forbidden)
        );
        let short = SeatIssuer::generate(CLUSTER, now, Duration::from_secs(3600)).unwrap();
        assert!(!short.can_issue(now));
        assert_eq!(
            short.issue(CLUSTER, NODE, &good, now),
            Err(PkiError::Unavailable)
        );
        assert!(short.should_rotate(now));
        assert!(!issuer.should_rotate(now));
        assert_eq!(fingerprint(b"").len(), 64);
    }

    fn pump(
        client: &mut rustls::ClientConnection,
        server: &mut rustls::ServerConnection,
    ) -> Result<(), rustls::Error> {
        for _ in 0..20 {
            let mut buf = Vec::new();
            client.write_tls(&mut buf).unwrap();
            server.read_tls(&mut buf.as_slice()).unwrap();
            server.process_new_packets()?;
            let mut buf = Vec::new();
            server.write_tls(&mut buf).unwrap();
            client.read_tls(&mut buf.as_slice()).unwrap();
            client.process_new_packets()?;
            if !client.is_handshaking() && !server.is_handshaking() {
                return Ok(());
            }
        }
        Ok(())
    }

    #[test]
    fn pinned_leaf_is_self_signed_end_entity() {
        let ip: IpAddr = "10.0.0.5".parse().unwrap();
        let s = ServingIdentity::pinned(
            &[ip],
            &["racer-webhook.ns.svc".into()],
            SystemTime::now(),
            SERVING_LIFETIME,
        )
        .unwrap();
        assert_eq!(s.ca_der, s.leaf_der);
        let (_, c) = x509_parser::parse_x509_certificate(&s.ca_der).unwrap();
        assert!(!c.is_ca());
        assert_eq!(c.subject(), c.issuer());
        s.server_config(None).unwrap();
    }

    #[test]
    fn mtls_with_pinned_serving_ca_and_root_swap() {
        let now = SystemTime::now();
        let ip: IpAddr = "10.0.0.5".parse().unwrap();
        let serving = ServingIdentity::generate(
            &[ip],
            &["racer-webhook.ns.svc".into()],
            now,
            SERVING_LIFETIME,
        )
        .unwrap();
        let issuer = SeatIssuer::generate(CLUSTER, now, ROOT_LIFETIME).unwrap();
        let (key, csr) = csr();
        let chain = issuer.issue(CLUSTER, NODE, &csr, now).unwrap();
        let verifier = DynamicClientVerifier::new();
        verifier.set_roots(&[issuer.der().to_vec()]);
        let server_config = serving.server_config(Some(verifier.clone())).unwrap();
        let client_config = |with_cert: bool| {
            let mut roots = rustls::RootCertStore::empty();
            roots
                .add(CertificateDer::from(serving.ca_der.clone()))
                .unwrap();
            let b = rustls::ClientConfig::builder_with_provider(Arc::new(
                rustls::crypto::ring::default_provider(),
            ))
            .with_safe_default_protocol_versions()
            .unwrap()
            .with_root_certificates(roots);
            Arc::new(if with_cert {
                b.with_client_auth_cert(
                    chain
                        .iter()
                        .map(|c| CertificateDer::from(c.clone()))
                        .collect(),
                    PrivateKeyDer::Pkcs8(key.serialize_der().into()),
                )
                .unwrap()
            } else {
                b.with_no_client_auth()
            })
        };
        let name = rustls::pki_types::ServerName::IpAddress(ip.into());
        for with_cert in [true, false] {
            let mut c =
                rustls::ClientConnection::new(client_config(with_cert), name.clone()).unwrap();
            let mut s = rustls::ServerConnection::new(server_config.clone()).unwrap();
            pump(&mut c, &mut s).unwrap();
            c.writer().write_all(b"x").unwrap();
            pump(&mut c, &mut s).unwrap();
            let mut got = [0u8; 1];
            s.reader().read_exact(&mut got).unwrap();
            assert_eq!(
                s.peer_certificates().map(<[_]>::len),
                with_cert.then_some(2)
            );
        }
        // After the root is removed the certificate fails the handshake.
        verifier.set_roots(&[]);
        let mut c = rustls::ClientConnection::new(client_config(true), name).unwrap();
        let mut s = rustls::ServerConnection::new(server_config).unwrap();
        assert!(pump(&mut c, &mut s).is_err() || s.peer_certificates().is_none());
    }
}
