// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

use super::*;
use crate::{
    cache::{self, http_metadata, peer_wire as wire},
    metadata::{Checksum, Metadata},
    tls,
};
use openssl::{
    asn1::Asn1Time,
    bn::{BigNum, MsbOption},
    ec::{EcGroup, EcKey},
    hash::MessageDigest,
    nid::Nid,
    pkey::PKey,
    x509::{X509, X509NameBuilder, extension::SubjectAlternativeName},
};

pub const VOLUME: &str = "transport-benchmark";
pub fn identity(server: bool) -> tls::PeerIdentity {
    tls::PeerIdentity::new(
        &"01".repeat(32),
        &if server { "02" } else { "03" }.repeat(32),
        "benchmark",
    )
    .unwrap()
}
pub fn tls(server: bool) -> io::Result<tls::TlsContext> {
    let peer = identity(server);
    let claims = tls::SignedClaims {
        version: 1,
        namespace: "benchmark".into(),
        identity: tls::ProcessIdentity {
            kind: "node".into(),
            universe: peer.universe,
            node: peer.node,
            pod_uid: peer.pod_uid,
            boot_id: "04".repeat(32),
            pod_name: "benchmark".into(),
            container_id: String::new(),
        },
    }
    .uri()?;
    let generate = || -> Result<_, openssl::error::ErrorStack> {
        let group = EcGroup::from_curve_name(Nid::X9_62_PRIME256V1)?;
        let key = PKey::from_ec_key(EcKey::generate(&group)?)?;
        let mut name = X509NameBuilder::new()?;
        name.append_entry_by_text("CN", "Racer development benchmark")?;
        let name = name.build();
        let mut cert = X509::builder()?;
        cert.set_version(2)?;
        let mut serial = BigNum::new()?;
        serial.rand(128, MsbOption::MAYBE_ZERO, false)?;
        let serial = serial.to_asn1_integer()?;
        cert.set_serial_number(&serial)?;
        cert.set_subject_name(&name)?;
        cert.set_issuer_name(&name)?;
        cert.set_pubkey(&key)?;
        let before = Asn1Time::days_from_now(0)?;
        let after = Asn1Time::days_from_now(1)?;
        cert.set_not_before(&before)?;
        cert.set_not_after(&after)?;
        let san = SubjectAlternativeName::new()
            .uri(&claims)
            .build(&cert.x509v3_context(None, None))?;
        cert.append_extension(san)?;
        cert.append_extension(
            openssl::x509::extension::ExtendedKeyUsage::new()
                .server_auth()
                .client_auth()
                .build()?,
        )?;
        cert.sign(&key, MessageDigest::sha256())?;
        Ok((cert.build().to_pem()?, key.private_key_to_pem_pkcs8()?))
    };
    let (cert, key) = generate().map_err(io::Error::other)?;
    let key = zeroize::Zeroizing::new(key);
    tls::TlsContext::benchmark(&cert, &key)
}

pub fn pattern(i: usize) -> u8 {
    (i as u8).wrapping_mul(31).wrapping_add((i >> 12) as u8)
}
pub fn record() -> Metadata {
    Metadata {
        content_type: Default::default(),
        checksum: Checksum([9; 32]),
        len: BUFFER_SIZE as u64,
        expires: u64::MAX,
    }
}
pub struct Fixture {
    pub request: cache::UpstreamRequest,
    pub key: [u8; 32],
    pub kind: Kind,
}
impl Fixture {
    pub fn new(kind: Kind) -> Self {
        let request = cache::benchmark_request(kind == Kind::Metadata, record());
        let key = match &request {
            cache::UpstreamRequest::PeerPage(p) => *p.key(),
            cache::UpstreamRequest::PeerMetadata(m) => m.key(),
            _ => unreachable!(),
        };
        Self { request, key, kind }
    }
    pub fn descriptor(&self, deadline: Instant) -> io::Result<Vec<u8>> {
        // Benchmark fidelity: RD01/RR01/RB01 are real codecs, including a routed
        // cursor and the same bounded remaining budget used by peer requests.
        let mut bytes = b"RR01".to_vec();
        bytes.extend(
            crate::routing::Cursor {
                algorithm: crate::routing::Algorithm::Canonical,
                identity: [7; 32],
                source: 0,
                owner: 1,
                attempt: 0,
                position: 1,
            }
            .encode(),
        );
        bytes.extend(wire::descriptor(&self.request)?);
        let bytes = wire::with_budget(
            bytes,
            deadline
                .saturating_duration_since(Instant::now())
                .saturating_sub(Duration::from_millis(10)),
        )?;
        wire::with_chain(
            bytes,
            [0; 32],
            wire::MAX_HOPS - 1,
            (wire::MAX_WORK - 1) / 2,
            1,
        )
    }
    pub fn fields(&self, deadline: Instant) -> io::Result<Vec<(&'static str, String)>> {
        let bytes = self.descriptor(deadline)?;
        let mut nonce = [0; 16];
        getrandom::getrandom(&mut nonce).map_err(|e| io::Error::other(e.to_string()))?;
        Ok(vec![
            ("X-Racer-Fault", wire::hex(&bytes)),
            ("X-Racer-Volume", VOLUME.into()),
            (
                "X-Racer-Attempt",
                format!(
                    "{}{}",
                    wire::hex(
                        crate::authorization::binding(&bytes, &Default::default()).as_bytes()
                    ),
                    wire::hex(&nonce)
                ),
            ),
        ])
    }
    pub fn validate_descriptor(&self, bytes: &[u8]) -> io::Result<()> {
        let (_, descriptor) = wire::routed_descriptor(bytes).map_err(cache::Error::into_io)?;
        if descriptor.target() != "/transport-benchmark/object" {
            return Err(invalid("wrong fixture target"));
        }
        // Decode with production code, then require the exact prepared descriptor.
        let (inner, _) = wire::budget_descriptor(bytes)?;
        if inner.len() < 4 + crate::routing::Cursor::LEN
            || inner[4 + crate::routing::Cursor::LEN..] != wire::descriptor(&self.request)?
        {
            return Err(invalid("wrong fixture descriptor"));
        }
        Ok(())
    }
    pub fn validate_request(&self, headers: crate::http::Headers<'_>) -> io::Result<()> {
        let text = http_metadata::text;
        let bytes =
            wire::unhex(text(headers, "x-racer-fault")?.ok_or_else(|| invalid("missing fault"))?)?;
        self.validate_descriptor(&bytes)?;
        let attempt =
            text(headers, "x-racer-attempt")?.ok_or_else(|| invalid("missing attempt"))?;
        if attempt.len() != 96
            || !attempt.bytes().all(|b| b.is_ascii_hexdigit())
            || !attempt.starts_with(&wire::hex(
                crate::authorization::binding(&bytes, &Default::default()).as_bytes(),
            ))
            || text(headers, "x-racer-volume")? != Some(VOLUME)
        {
            return Err(invalid("invalid attempt or volume"));
        }
        Ok(())
    }
    pub fn validate_response(
        &self,
        headers: crate::http::Headers<'_>,
        body: Option<&[u8]>,
    ) -> io::Result<()> {
        http_metadata::peer_checksum(headers, record().checksum).map_err(cache::Error::into_io)?;
        http_metadata::identity_encoding(headers)?;
        let crc = http_metadata::checksum(headers)?.ok_or_else(|| invalid("missing CRC64"))?;
        if self.kind == Kind::Metadata {
            let bytes = body.ok_or_else(|| invalid("missing metadata"))?;
            if crate::allocator::crc64(bytes) != crc || Metadata::from_bytes(bytes)? != record() {
                return Err(invalid("metadata mismatch"));
            }
        }
        Ok(())
    }
    pub fn validate_body(&self, bytes: &[u8]) -> io::Result<()> {
        if bytes.len() != self.kind.bytes()
            || if self.kind == Kind::Metadata {
                bytes != record().to_bytes()
            } else {
                bytes.iter().enumerate().any(|(i, b)| *b != pattern(i))
            }
        {
            return Err(invalid("payload validation failed"));
        }
        Ok(())
    }
}
