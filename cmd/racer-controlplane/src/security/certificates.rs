// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

use super::*;
use anyhow::{Context, bail};
use openssl::{
    asn1::{Asn1Integer, Asn1Time},
    bn::{BigNum, MsbOption},
    ec::{EcGroup, EcKey},
    hash::MessageDigest,
    nid::Nid,
    pkey::{Id, PKey, Private, Public},
    stack::Stack,
    x509::{
        X509, X509NameBuilder, X509Req, X509StoreContext,
        extension::{
            AuthorityKeyIdentifier, BasicConstraints, ExtendedKeyUsage, KeyUsage,
            SubjectAlternativeName, SubjectKeyIdentifier,
        },
        store::X509StoreBuilder,
    },
};

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct TrustBundle {
    pub version: u32,
    pub generation: u64,
    pub active: String,
    pub certificates: String,
}

impl TrustBundle {
    pub fn json(&self) -> Vec<u8> {
        // All strings are restricted ASCII hex or PEM. serde's encoding matches
        // encoding/json, including field order and the absence of a final LF.
        serde_json::to_vec(self).expect("primitive trust schema")
    }

    pub fn digest(&self) -> String {
        digest(&self.json())
    }

    pub fn parse(bytes: &[u8]) -> Result<Self> {
        let bundle: Self = serde_json::from_slice(bytes)?;
        ensure!(
            bundle.version == 1 && bundle.generation > 0 && hex_id(&bundle.active),
            "invalid trust metadata"
        );
        let roots = parse_certificates(bundle.certificates.as_bytes())?;
        ensure!(roots.len() <= 2, "too many trust roots");
        let mut ids = std::collections::BTreeSet::new();
        for root in roots {
            validate_root(&root)?;
            ensure!(ids.insert(digest(&root.to_der()?)), "duplicate trust root");
        }
        ensure!(ids.contains(&bundle.active), "active root absent");
        ensure!(bytes == bundle.json(), "noncanonical trust publication");
        Ok(bundle)
    }
}

/// Strict PEM framing: OpenSSL's permissive scanning must not hide junk.
pub(crate) fn pem_blocks<'a>(bytes: &'a [u8], label: &str) -> Result<Vec<&'a [u8]>> {
    let text = std::str::from_utf8(bytes)?;
    let begin = format!("-----BEGIN {label}-----");
    let end = format!("-----END {label}-----");
    let mut remaining = text.trim();
    let mut blocks = Vec::new();
    while !remaining.is_empty() {
        ensure!(remaining.starts_with(&begin), "invalid PEM framing");
        let finish = remaining.find(&end).context("missing PEM end")? + end.len();
        let block = &remaining[..finish];
        ensure!(
            !block[begin.len()..block.len() - end.len()].contains(':'),
            "PEM headers unsupported"
        );
        blocks.push(block.as_bytes());
        remaining = remaining[finish..].trim();
    }
    ensure!(!blocks.is_empty(), "empty PEM");
    Ok(blocks)
}

pub(crate) fn parse_certificates(bytes: &[u8]) -> Result<Vec<X509>> {
    pem_blocks(bytes, "CERTIFICATE")?
        .into_iter()
        .map(|b| Ok(X509::from_pem(b)?))
        .collect()
}

fn validate_root(root: &X509) -> Result<()> {
    let key = root.public_key()?;
    ensure!(root.verify(&key)?, "root is not self-signed");
    ensure!(
        root.subject_name().to_der()? == root.issuer_name().to_der()?,
        "root issuer mismatch"
    );
    // OpenSSL's verification enforces CA constraints when this root signs a
    // chain; explicitly inspect the constraints even for an unused trust root.
    let der = root.to_der()?;
    let (_, parsed) = x509_parser::parse_x509_certificate(&der)
        .map_err(|e| anyhow::anyhow!("invalid root: {e}"))?;
    ensure!(
        parsed.basic_constraints()?.is_some_and(|c| c.value.ca),
        "trust anchor is not a CA"
    );
    Ok(())
}

/// Secret-bearing type deliberately has no Debug implementation.
#[derive(Clone, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Authority {
    pub certificate: String,
    pub private_key: String,
    pub digest: String,
    pub last_issued_expiry: i64,
}

fn serial() -> Result<Asn1Integer> {
    let mut n = BigNum::new()?;
    n.rand(159, MsbOption::ONE, false)?;
    Ok(n.to_asn1_integer()?)
}

impl Authority {
    pub fn generate(now: i64, lifetime: i64, skew: i64) -> Result<Self> {
        ensure!(lifetime > 0 && skew >= 0, "invalid CA lifetime");
        let group = EcGroup::from_curve_name(Nid::X9_62_PRIME256V1)?;
        let key = PKey::from_ec_key(EcKey::generate(&group)?)?;
        let mut name = X509NameBuilder::new()?;
        name.append_entry_by_text("CN", "Racer root CA")?;
        let name = name.build();
        let mut cert = X509::builder()?;
        cert.set_version(2)?;
        cert.set_serial_number(serial()?.as_ref())?;
        cert.set_subject_name(&name)?;
        cert.set_issuer_name(&name)?;
        cert.set_pubkey(&key)?;
        cert.set_not_before(
            Asn1Time::from_unix(now.checked_sub(skew).context("time overflow")?)?.as_ref(),
        )?;
        cert.set_not_after(
            Asn1Time::from_unix(now.checked_add(lifetime).context("time overflow")?)?.as_ref(),
        )?;
        cert.append_extension(BasicConstraints::new().critical().ca().pathlen(0).build()?)?;
        cert.append_extension(
            KeyUsage::new()
                .critical()
                .key_cert_sign()
                .crl_sign()
                .build()?,
        )?;
        cert.append_extension(
            SubjectKeyIdentifier::new().build(&cert.x509v3_context(None, None))?,
        )?;
        cert.sign(&key, MessageDigest::sha256())?;
        let cert = cert.build();
        Ok(Self {
            digest: digest(&cert.to_der()?),
            certificate: String::from_utf8(cert.to_pem()?)?,
            private_key: String::from_utf8(key.private_key_to_pem_pkcs8()?)?,
            last_issued_expiry: 0,
        })
    }

    pub(crate) fn parse(&self) -> Result<(X509, PKey<Private>)> {
        let certs = parse_certificates(self.certificate.as_bytes())?;
        ensure!(certs.len() == 1, "CA must have one certificate");
        let cert = certs.into_iter().next().unwrap();
        validate_root(&cert)?;
        ensure!(digest(&cert.to_der()?) == self.digest, "CA digest mismatch");
        ensure!(
            pem_blocks(self.private_key.as_bytes(), "PRIVATE KEY")?.len() == 1,
            "multiple CA keys"
        );
        let key = PKey::private_key_from_pem(self.private_key.as_bytes())?;
        ensure!(
            key.public_eq(cert.public_key()?.as_ref()),
            "CA key mismatch"
        );
        ensure!(
            Asn1Time::from_unix(self.last_issued_expiry)?.compare(cert.not_after())?
                != std::cmp::Ordering::Greater,
            "invalid expiry watermark"
        );
        Ok((cert, key))
    }
}

/// Validates the PKCS#10 signature before returning only its public key. Neither
/// requested names nor any requested extensions are copied to the certificate.
pub fn validate_csr(bytes: &[u8]) -> Result<PKey<Public>> {
    ensure!(bytes.len() <= 32768, "CSR too large");
    let req = if bytes.starts_with(b"-----") {
        ensure!(
            pem_blocks(bytes, "CERTIFICATE REQUEST")?.len() == 1,
            "multiple CSRs"
        );
        X509Req::from_pem(bytes)?
    } else {
        let req = X509Req::from_der(bytes)?;
        ensure!(req.to_der()? == bytes, "trailing or noncanonical CSR DER");
        req
    };
    let key = req.public_key()?;
    ensure!(req.verify(&key)?, "invalid CSR signature");
    match key.id() {
        Id::RSA => ensure!(key.bits() >= 2048, "RSA key must have at least 2048 bits"),
        Id::EC => ensure!(
            matches!(
                key.ec_key()?.group().curve_name(),
                Some(Nid::X9_62_PRIME256V1 | Nid::SECP384R1 | Nid::SECP521R1)
            ),
            "unsupported EC curve"
        ),
        Id::ED25519 => (),
        _ => bail!("unsupported CSR key"),
    }
    Ok(key)
}

pub struct LocalKey {
    pub csr_pem: Vec<u8>,
    pub key_pem: Vec<u8>,
}

pub fn generate_local_key() -> Result<LocalKey> {
    let group = EcGroup::from_curve_name(Nid::X9_62_PRIME256V1)?;
    let key = PKey::from_ec_key(EcKey::generate(&group)?)?;
    let mut csr = X509Req::builder()?;
    csr.set_pubkey(&key)?;
    csr.set_subject_name(&X509NameBuilder::new()?.build())?;
    csr.sign(&key, MessageDigest::sha256())?;
    Ok(LocalKey {
        csr_pem: csr.build().to_pem()?,
        key_pem: key.private_key_to_pem_pkcs8()?,
    })
}

pub(crate) fn sign_leaf(
    ca: &Authority,
    public_key: &PKey<Public>,
    identity: &Identity,
    namespace: &str,
    now: i64,
    lifetime: i64,
    skew: i64,
) -> Result<(Vec<u8>, i64)> {
    let (parent, key) = ca.parse()?;
    let uri = identity.uri()?;
    let expiry = now.checked_add(lifetime).context("expiry overflow")?;
    ensure!(lifetime > 0 && skew >= 0, "invalid leaf lifetime");
    ensure!(
        Asn1Time::from_unix(now)?.compare(parent.not_before())? != std::cmp::Ordering::Less,
        "CA not yet valid"
    );
    ensure!(
        Asn1Time::from_unix(expiry.checked_add(skew).context("expiry overflow")?)?
            .compare(parent.not_after())?
            == std::cmp::Ordering::Less,
        "CA cannot cover leaf lifetime"
    );
    let mut cert = X509::builder()?;
    cert.set_version(2)?;
    cert.set_serial_number(serial()?.as_ref())?;
    cert.set_subject_name(&X509NameBuilder::new()?.build())?;
    cert.set_issuer_name(parent.subject_name())?;
    cert.set_pubkey(public_key)?;
    cert.set_not_before(
        Asn1Time::from_unix(now.checked_sub(skew).context("time overflow")?)?.as_ref(),
    )?;
    cert.set_not_after(Asn1Time::from_unix(expiry)?.as_ref())?;
    cert.append_extension(BasicConstraints::new().critical().build()?)?;
    cert.append_extension(KeyUsage::new().critical().digital_signature().build()?)?;
    let mut eku = ExtendedKeyUsage::new();
    eku.server_auth();
    let mut san = SubjectAlternativeName::new();
    san.uri(&uri);
    if identity.kind == IdentityKind::Node {
        eku.client_auth();
    } else {
        ensure!(valid_namespace(namespace), "invalid namespace");
        san.dns(&format!("racer-controlplane.{namespace}.svc"));
    }
    cert.append_extension(eku.build()?)?;
    // An empty subject requires a critical SAN (RFC 5280 section 4.1.2.6).
    cert.append_extension(
        san.critical()
            .build(&cert.x509v3_context(Some(&parent), None))?,
    )?;
    cert.append_extension(
        AuthorityKeyIdentifier::new()
            .keyid(true)
            .build(&cert.x509v3_context(Some(&parent), None))?,
    )?;
    cert.sign(&key, MessageDigest::sha256())?;
    Ok((cert.build().to_pem()?, expiry))
}

pub(crate) fn valid_namespace(s: &str) -> bool {
    !s.is_empty()
        && s.len() <= 63
        && s.as_bytes()[0].is_ascii_alphanumeric()
        && s.as_bytes()[s.len() - 1].is_ascii_alphanumeric()
        && s.bytes()
            .all(|b| b.is_ascii_lowercase() || b.is_ascii_digit() || b == b'-')
}

pub(crate) fn leaf_uri(cert: &X509) -> Result<String> {
    let der = cert.to_der()?;
    let (_, parsed) = x509_parser::parse_x509_certificate(&der)
        .map_err(|e| anyhow::anyhow!("invalid leaf: {e}"))?;
    ensure!(!parsed.is_ca(), "CA used as leaf");
    let sans = cert.subject_alt_names().context("missing SAN")?;
    let uris: Vec<_> = sans.iter().filter_map(|s| s.uri()).collect();
    ensure!(uris.len() == 1, "expected exactly one URI");
    Ok(uris[0].into())
}

/// Independent chain verification for snapshot installation and issuer binding.
/// TLS itself is verified by rustls; this never replaces handshake verification.
pub(crate) fn leaf_root(cert: &X509, bundle: &TrustBundle, server: bool) -> Result<String> {
    for root in parse_certificates(bundle.certificates.as_bytes())? {
        let mut store = X509StoreBuilder::new()?;
        store.add_cert(root.clone())?;
        store.set_purpose(if server {
            openssl::x509::X509PurposeId::SSL_SERVER
        } else {
            openssl::x509::X509PurposeId::SSL_CLIENT
        })?;
        let mut ctx = X509StoreContext::new()?;
        if ctx.init(&store.build(), cert, Stack::new()?.as_ref(), |ctx| {
            ctx.verify_cert()
        })? {
            return Ok(digest(&root.to_der()?));
        }
    }
    bail!("leaf not valid under trust bundle")
}
