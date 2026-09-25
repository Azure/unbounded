//! Strict bounded JSON and canonical publication content shared with Go.
use super::*;
use crate::topology::rails::{RailId, RailMapping};
use base64::{Engine, engine::general_purpose::STANDARD};
use serde::{
    Deserialize, Serialize,
    de::{self, MapAccess, SeqAccess, Visitor},
};
use serde_json::Value;
use sha2::{Digest, Sha256};
use std::{collections::HashSet, fmt, io::Read, net::SocketAddr, num::NonZeroU32};
use x509_parser::prelude::{FromDer, X509Certificate, X509CertificationRequest};

type Result<T> = std::result::Result<T, ProtocolFailure>;

#[cfg(test)]
#[path = "codec/tests.rs"]
mod tests;

// This visitor runs before typed deserialization, including inside ignored fields.
// serde_json::Value alone silently overwrites duplicate object fields.
struct StrictValue(Value);
impl<'de> Deserialize<'de> for StrictValue {
    fn deserialize<D: serde::Deserializer<'de>>(d: D) -> std::result::Result<Self, D::Error> {
        struct StrictVisitor;
        impl<'de> Visitor<'de> for StrictVisitor {
            type Value = StrictValue;
            fn expecting(&self, f: &mut fmt::Formatter) -> fmt::Result {
                f.write_str("JSON value")
            }
            fn visit_bool<E: de::Error>(self, v: bool) -> std::result::Result<Self::Value, E> {
                Ok(StrictValue(v.into()))
            }
            fn visit_i64<E: de::Error>(self, v: i64) -> std::result::Result<Self::Value, E> {
                Ok(StrictValue(v.into()))
            }
            fn visit_u64<E: de::Error>(self, v: u64) -> std::result::Result<Self::Value, E> {
                Ok(StrictValue(v.into()))
            }
            fn visit_f64<E: de::Error>(self, v: f64) -> std::result::Result<Self::Value, E> {
                Ok(StrictValue(v.into()))
            }
            fn visit_str<E: de::Error>(self, v: &str) -> std::result::Result<Self::Value, E> {
                Ok(StrictValue(v.into()))
            }
            fn visit_string<E: de::Error>(self, v: String) -> std::result::Result<Self::Value, E> {
                Ok(StrictValue(v.into()))
            }
            fn visit_unit<E: de::Error>(self) -> std::result::Result<Self::Value, E> {
                Ok(StrictValue(Value::Null))
            }
            fn visit_seq<A: SeqAccess<'de>>(
                self,
                mut a: A,
            ) -> std::result::Result<Self::Value, A::Error> {
                let mut values = Vec::new();
                while let Some(StrictValue(v)) = a.next_element()? {
                    values.push(v);
                }
                Ok(StrictValue(Value::Array(values)))
            }
            fn visit_map<A: MapAccess<'de>>(
                self,
                mut a: A,
            ) -> std::result::Result<Self::Value, A::Error> {
                let mut values = serde_json::Map::new();
                while let Some(key) = a.next_key::<String>()? {
                    if values.contains_key(&key) {
                        return Err(de::Error::custom("duplicate field"));
                    }
                    let StrictValue(v) = a.next_value()?;
                    values.insert(key, v);
                }
                Ok(StrictValue(Value::Object(values)))
            }
        }
        d.deserialize_any(StrictVisitor)
    }
}

fn bounded_json(mut r: impl Read, max: usize) -> Result<Value> {
    let mut bytes = Vec::new();
    r.by_ref()
        .take(max as u64 + 1)
        .read_to_end(&mut bytes)
        .map_err(|_| ProtocolFailure::InvalidRequest)?;
    if bytes.len() > max {
        return Err(ProtocolFailure::TooLarge);
    }
    // Bound nesting identically to Go before entering serde's recursive visitor.
    let mut depth = 0usize;
    let mut quoted = false;
    let mut escape = false;
    for &b in &bytes {
        if quoted {
            if escape {
                escape = false;
            } else if b == b'\\' {
                escape = true;
            } else if b == b'"' {
                quoted = false;
            }
        } else {
            match b {
                b'"' => quoted = true,
                b'{' | b'[' => {
                    depth += 1;
                    if depth > 65 {
                        return Err(ProtocolFailure::InvalidRequest);
                    }
                }
                b'}' | b']' => {
                    depth = depth.saturating_sub(1);
                }
                _ => (),
            }
        }
    }
    let mut d = serde_json::Deserializer::from_slice(&bytes);
    let StrictValue(value) =
        StrictValue::deserialize(&mut d).map_err(|_| ProtocolFailure::InvalidRequest)?;
    d.end().map_err(|_| ProtocolFailure::InvalidRequest)?;
    Ok(value)
}

#[derive(Serialize, Deserialize)]
struct RailDto {
    rail: u16,
    fabric: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    numa_node: Option<u32>,
}
#[derive(Serialize, Deserialize)]
struct MemberDto {
    node: String,
    shares: u32,
    peer_endpoint: String,
    rails: Vec<RailDto>,
    alignment_enabled: bool,
}
#[derive(Serialize, Deserialize)]
struct CacheDto {
    id: String,
    name: String,
    client_socket: String,
    origin_socket: String,
    socket_mode: u32,
}
#[derive(Serialize, Deserialize)]
struct PublicationDto {
    schema_version: u32,
    cluster: String,
    sequence: String,
    membership_version: String,
    members: Vec<MemberDto>,
    caches: Vec<CacheDto>,
}

fn uuid(s: &str) -> bool {
    s.len() == 36
        && s.bytes().enumerate().all(|(i, b)| {
            if [8, 13, 18, 23].contains(&i) {
                b == b'-'
            } else {
                b.is_ascii_digit() || (b'a'..=b'f').contains(&b)
            }
        })
}

fn counter(s: &str) -> Result<u64> {
    let n: u64 = s.parse().map_err(|_| ProtocolFailure::InvalidRequest)?;
    if n == 0 || n.to_string() != s {
        return Err(ProtocolFailure::InvalidRequest);
    }
    Ok(n)
}

fn paths(name: &str) -> Result<(String, String)> {
    if name.is_empty()
        || name.len() > 253
        || !name.split('.').all(|label| {
            !label.is_empty()
                && label.len() <= 63
                && label.bytes().enumerate().all(|(i, b)| {
                    b.is_ascii_lowercase()
                        || b.is_ascii_digit()
                        || (b == b'-' && i > 0 && i + 1 < label.len())
                })
        })
    {
        return Err(ProtocolFailure::InvalidRequest);
    }
    let client = format!("/run/racer/{name}/client/socket");
    let origin = format!("/run/racer/{name}/origin/socket");
    if client.len() > 107 || origin.len() > 107 {
        return Err(ProtocolFailure::InvalidRequest);
    }
    Ok((client, origin))
}

impl PublicationDto {
    fn validate(&self, counters: bool) -> Result<()> {
        if self.schema_version != SCHEMA_VERSION {
            return Err(ProtocolFailure::UnsupportedVersion);
        }
        if !uuid(&self.cluster) {
            return Err(ProtocolFailure::InvalidRequest);
        }
        if counters {
            counter(&self.sequence)?;
            counter(&self.membership_version)?;
        }
        if self.members.len() > MAX_MEMBERS {
            return Err(ProtocolFailure::TooLarge);
        }
        let mut nodes = HashSet::new();
        for m in &self.members {
            if !uuid(&m.node) || !nodes.insert(&m.node) || m.shares == 0 {
                return Err(ProtocolFailure::InvalidRequest);
            }
            let endpoint: SocketAddr = m
                .peer_endpoint
                .parse()
                .map_err(|_| ProtocolFailure::InvalidRequest)?;
            if endpoint.port() == 0 {
                return Err(ProtocolFailure::InvalidRequest);
            }
            let mut rails = HashSet::new();
            for r in &m.rails {
                if !rails.insert(r.rail)
                    || r.fabric.is_empty()
                    || r.fabric.contains(['\0', '\r', '\n'])
                {
                    return Err(ProtocolFailure::InvalidRequest);
                }
            }
        }
        let mut ids = HashSet::new();
        let mut names = HashSet::new();
        for c in &self.caches {
            if !uuid(&c.id) || !ids.insert(&c.id) || !names.insert(&c.name) || c.socket_mode > 0o777
            {
                return Err(ProtocolFailure::InvalidRequest);
            }
            let (client, origin) = paths(&c.name)?;
            if c.client_socket != client || c.origin_socket != origin {
                return Err(ProtocolFailure::InvalidRequest);
            }
        }
        Ok(())
    }
    fn sort(&mut self) {
        self.members.sort_by(|a, b| a.node.cmp(&b.node));
        self.caches.sort_by(|a, b| a.id.cmp(&b.id));
        for m in &mut self.members {
            m.rails.sort_by_key(|r| r.rail);
        }
    }
    fn from_publication(p: &Publication) -> Result<Self> {
        if p.members.len() > MAX_MEMBERS {
            return Err(ProtocolFailure::TooLarge);
        }
        let mut remaining = MAX_PUBLICATION_BYTES;
        let mut consume = |n: usize| -> Result<()> {
            remaining = remaining.checked_sub(n).ok_or(ProtocolFailure::TooLarge)?;
            Ok(())
        };
        for m in &p.members {
            consume(m.node.0.len())?;
            consume(m.peer_endpoint.len())?;
            for r in &m.rails {
                consume(r.fabric.len().saturating_add(1))?;
            }
        }
        for c in &p.caches {
            consume(c.id.0.len())?;
            consume(c.name.len())?;
            consume(c.client_socket.as_os_str().len())?;
            consume(c.origin_socket.as_os_str().len())?;
        }
        Ok(Self {
            schema_version: p.schema_version,
            cluster: p.cluster.0.clone(),
            sequence: p.sequence.0.to_string(),
            membership_version: p.membership_version.0.to_string(),
            members: p
                .members
                .iter()
                .map(|m| {
                    Ok(MemberDto {
                        node: m.node.0.clone(),
                        shares: m.shares.get(),
                        peer_endpoint: m.peer_endpoint.clone(),
                        alignment_enabled: m.alignment_enabled,
                        rails: m
                            .rails
                            .iter()
                            .map(|r| {
                                Ok(RailDto {
                                    rail: r.rail.0,
                                    fabric: r.fabric.clone(),
                                    numa_node: r
                                        .numa_node
                                        .map(u32::try_from)
                                        .transpose()
                                        .map_err(|_| ProtocolFailure::InvalidRequest)?,
                                })
                            })
                            .collect::<Result<_>>()?,
                    })
                })
                .collect::<Result<_>>()?,
            caches: p
                .caches
                .iter()
                .map(|c| {
                    Ok(CacheDto {
                        id: c.id.0.clone(),
                        name: c.name.clone(),
                        client_socket: c
                            .client_socket
                            .to_str()
                            .ok_or(ProtocolFailure::InvalidRequest)?
                            .into(),
                        origin_socket: c
                            .origin_socket
                            .to_str()
                            .ok_or(ProtocolFailure::InvalidRequest)?
                            .into(),
                        socket_mode: c.socket_mode,
                    })
                })
                .collect::<Result<_>>()?,
        })
    }
    fn into_publication(self) -> Publication {
        Publication {
            schema_version: self.schema_version,
            cluster: ClusterId(self.cluster),
            sequence: PublicationSequence(self.sequence.parse().expect("validated counter")),
            membership_version: MembershipVersion(
                self.membership_version.parse().expect("validated counter"),
            ),
            members: self
                .members
                .into_iter()
                .map(|m| Member {
                    node: NodeId(m.node),
                    shares: NonZeroU32::new(m.shares).expect("validated shares"),
                    peer_endpoint: m.peer_endpoint,
                    alignment_enabled: m.alignment_enabled,
                    rails: m
                        .rails
                        .into_iter()
                        .map(|r| RailMapping {
                            rail: RailId(r.rail),
                            fabric: r.fabric,
                            numa_node: r.numa_node.map(|n| n as usize),
                        })
                        .collect(),
                })
                .collect(),
            caches: self
                .caches
                .into_iter()
                .map(|c| CacheDefinition {
                    id: CacheId(c.id),
                    name: c.name,
                    client_socket: c.client_socket.into(),
                    origin_socket: c.origin_socket.into(),
                    socket_mode: c.socket_mode,
                })
                .collect(),
        }
    }
}

pub fn decode_publication(r: impl Read) -> Result<Publication> {
    let value = bounded_json(r, MAX_PUBLICATION_BYTES)?;
    // Optional means absent, never null. Serde Option alone accepts both.
    if let Some(members) = value.get("members").and_then(Value::as_array) {
        if members.len() > MAX_MEMBERS {
            return Err(ProtocolFailure::TooLarge);
        }
        for member in members {
            if let Some(rails) = member.get("rails").and_then(Value::as_array)
                && rails
                    .iter()
                    .any(|r| r.get("numa_node").is_some_and(Value::is_null))
            {
                return Err(ProtocolFailure::InvalidRequest);
            }
        }
    }
    let mut dto: PublicationDto =
        serde_json::from_value(value).map_err(|_| ProtocolFailure::InvalidRequest)?;
    dto.validate(true)?;
    dto.sort();
    Ok(dto.into_publication())
}

fn encode(v: &impl Serialize, max: usize) -> Result<Vec<u8>> {
    let text = serde_json::to_string(v).map_err(|_| ProtocolFailure::InvalidRequest)?;
    // Go's JSON encoder escapes these two Unicode separators even with HTML
    // escaping disabled. All other non-ASCII text is literal UTF-8.
    let bytes = text
        .replace('\u{2028}', "\\u2028")
        .replace('\u{2029}', "\\u2029")
        .into_bytes();
    if bytes.len() > max {
        return Err(ProtocolFailure::TooLarge);
    }
    Ok(bytes)
}

pub fn encode_publication(p: &Publication) -> Result<Vec<u8>> {
    let mut dto = PublicationDto::from_publication(p)?;
    dto.validate(true)?;
    dto.sort();
    encode(&dto, MAX_PUBLICATION_BYTES)
}

/// Counter-free JSON. Candidates may have zero counters before version CAS.
pub fn canonical_content(p: &Publication) -> Result<(Vec<u8>, Vec<u8>)> {
    let mut dto = PublicationDto::from_publication(p)?;
    dto.validate(false)?;
    dto.sort();
    #[derive(Serialize)]
    struct Content<'a> {
        schema_version: u32,
        cluster: &'a str,
        members: &'a [MemberDto],
        caches: &'a [CacheDto],
    }
    #[derive(Serialize)]
    struct Membership<'a> {
        schema_version: u32,
        cluster: &'a str,
        members: &'a [MemberDto],
    }
    Ok((
        encode(
            &Content {
                schema_version: dto.schema_version,
                cluster: &dto.cluster,
                members: &dto.members,
                caches: &dto.caches,
            },
            MAX_PUBLICATION_BYTES,
        )?,
        encode(
            &Membership {
                schema_version: dto.schema_version,
                cluster: &dto.cluster,
                members: &dto.members,
            },
            MAX_PUBLICATION_BYTES,
        )?,
    ))
}

pub fn content_hashes(p: &Publication) -> Result<(String, String)> {
    let (content, membership) = canonical_content(p)?;
    Ok((
        format!("{:x}", Sha256::digest(content)),
        format!("{:x}", Sha256::digest(membership)),
    ))
}

fn header(version: u32, cluster: &str) -> Result<()> {
    if version != SCHEMA_VERSION {
        return Err(ProtocolFailure::UnsupportedVersion);
    }
    if !uuid(cluster) {
        return Err(ProtocolFailure::InvalidRequest);
    }
    Ok(())
}
fn bytes(s: &str) -> Result<Vec<u8>> {
    let b = STANDARD
        .decode(s)
        .map_err(|_| ProtocolFailure::InvalidRequest)?;
    if STANDARD.encode(&b) != s {
        return Err(ProtocolFailure::InvalidRequest);
    }
    Ok(b)
}
fn certificates(certs: &[Vec<u8>], max: usize) -> Result<()> {
    if certs.is_empty() {
        return Err(ProtocolFailure::InvalidRequest);
    }
    let mut total = 0;
    for cert in certs {
        if cert.len() > max - total {
            return Err(ProtocolFailure::TooLarge);
        }
        total += cert.len();
        let (rest, _) =
            X509Certificate::from_der(cert).map_err(|_| ProtocolFailure::InvalidRequest)?;
        if !rest.is_empty() {
            return Err(ProtocolFailure::InvalidRequest);
        }
    }
    Ok(())
}
#[derive(Serialize, Deserialize)]
struct EnrollmentRequestDto {
    schema_version: u32,
    cluster: String,
    enrollment: String,
    csr_der: String,
}
#[derive(Serialize, Deserialize)]
struct EnrollmentResponseDto {
    schema_version: u32,
    cluster: String,
    node: String,
    enrollment: String,
    certificate_chain: Vec<String>,
}

fn validate_request(v: &EnrollmentRequest) -> Result<()> {
    header(v.schema_version, &v.cluster.0)?;
    if !uuid(&v.enrollment.0) {
        return Err(ProtocolFailure::InvalidRequest);
    }
    if v.csr_der.len() > MAX_ENROLLMENT_BYTES {
        return Err(ProtocolFailure::TooLarge);
    }
    let (rest, _) = X509CertificationRequest::from_der(&v.csr_der)
        .map_err(|_| ProtocolFailure::InvalidRequest)?;
    if !rest.is_empty() {
        return Err(ProtocolFailure::InvalidRequest);
    }
    Ok(())
}
fn validate_response(v: &EnrollmentResponse) -> Result<()> {
    header(v.schema_version, &v.cluster.0)?;
    if !uuid(&v.node.0) || !uuid(&v.enrollment.0) {
        return Err(ProtocolFailure::InvalidRequest);
    }
    certificates(&v.certificate_chain, MAX_ENROLLMENT_BYTES)
}
pub fn decode_enrollment_request(r: impl Read) -> Result<EnrollmentRequest> {
    let d: EnrollmentRequestDto = serde_json::from_value(bounded_json(r, MAX_ENROLLMENT_BYTES)?)
        .map_err(|_| ProtocolFailure::InvalidRequest)?;
    let v = EnrollmentRequest {
        schema_version: d.schema_version,
        cluster: ClusterId(d.cluster),
        enrollment: EnrollmentId(d.enrollment),
        csr_der: bytes(&d.csr_der)?,
    };
    validate_request(&v)?;
    Ok(v)
}
pub fn encode_enrollment_request(v: &EnrollmentRequest) -> Result<Vec<u8>> {
    validate_request(v)?;
    encode(
        &EnrollmentRequestDto {
            schema_version: v.schema_version,
            cluster: v.cluster.0.clone(),
            enrollment: v.enrollment.0.clone(),
            csr_der: STANDARD.encode(&v.csr_der),
        },
        MAX_ENROLLMENT_BYTES,
    )
}
pub fn decode_enrollment_response(r: impl Read) -> Result<EnrollmentResponse> {
    let d: EnrollmentResponseDto = serde_json::from_value(bounded_json(r, MAX_ENROLLMENT_BYTES)?)
        .map_err(|_| ProtocolFailure::InvalidRequest)?;
    let v = EnrollmentResponse {
        schema_version: d.schema_version,
        cluster: ClusterId(d.cluster),
        node: NodeId(d.node),
        enrollment: EnrollmentId(d.enrollment),
        certificate_chain: d
            .certificate_chain
            .iter()
            .map(|s| bytes(s))
            .collect::<Result<_>>()?,
    };
    validate_response(&v)?;
    Ok(v)
}
pub fn encode_enrollment_response(v: &EnrollmentResponse) -> Result<Vec<u8>> {
    validate_response(v)?;
    encode(
        &EnrollmentResponseDto {
            schema_version: v.schema_version,
            cluster: v.cluster.0.clone(),
            node: v.node.0.clone(),
            enrollment: v.enrollment.0.clone(),
            certificate_chain: v
                .certificate_chain
                .iter()
                .map(|b| STANDARD.encode(b))
                .collect(),
        },
        MAX_ENROLLMENT_BYTES,
    )
}

#[derive(Serialize, Deserialize)]
struct KeyDto {
    cache: String,
    id: String,
    purpose: String,
    state: String,
    material: String,
}
#[derive(Serialize, Deserialize)]
struct BundleDto {
    schema_version: u32,
    cluster: String,
    generation: String,
    peer_trust_roots: Vec<String>,
    cache_keys: Vec<KeyDto>,
}

fn purpose(p: CacheKeyPurpose) -> &'static str {
    match p {
        CacheKeyPurpose::Page => "page",
        CacheKeyPurpose::OriginCredentials => "origin_credentials",
    }
}
fn state(s: CacheKeyState) -> &'static str {
    match s {
        CacheKeyState::Prepared => "prepared",
        CacheKeyState::Active => "active",
        CacheKeyState::Retiring => "retiring",
    }
}
fn validate_bundle(v: &KeyringBundle) -> Result<()> {
    header(v.schema_version, &v.cluster.0)?;
    if v.generation.0 == 0 {
        return Err(ProtocolFailure::InvalidRequest);
    }
    if v.cache_keys.len() > MAX_BUNDLE_BYTES / 32 {
        return Err(ProtocolFailure::TooLarge);
    }
    certificates(&v.peer_trust_roots, MAX_BUNDLE_BYTES)?;
    let mut roots = HashSet::new();
    for root in &v.peer_trust_roots {
        if !roots.insert(root) {
            return Err(ProtocolFailure::InvalidRequest);
        }
    }
    let mut keys = HashSet::new();
    let mut active = std::collections::HashMap::new();
    for k in &v.cache_keys {
        if !uuid(&k.key.cache.0)
            || !keys.insert((&k.key.cache.0, purpose(k.key.purpose), k.key.id.0))
        {
            return Err(ProtocolFailure::InvalidRequest);
        }
        let count = active
            .entry((&k.key.cache.0, purpose(k.key.purpose)))
            .or_insert(0);
        if k.state == CacheKeyState::Active {
            *count += 1;
        }
    }
    if active.values().any(|&n| n != 1) {
        return Err(ProtocolFailure::InvalidRequest);
    }
    Ok(())
}
pub fn decode_bundle(r: impl Read) -> Result<KeyringBundle> {
    let d: BundleDto = serde_json::from_value(bounded_json(r, MAX_BUNDLE_BYTES)?)
        .map_err(|_| ProtocolFailure::InvalidRequest)?;
    let v = KeyringBundle {
        schema_version: d.schema_version,
        cluster: ClusterId(d.cluster),
        generation: BundleGeneration(counter(&d.generation)?),
        peer_trust_roots: d
            .peer_trust_roots
            .iter()
            .map(|s| bytes(s))
            .collect::<Result<_>>()?,
        cache_keys: d
            .cache_keys
            .iter()
            .map(|k| {
                Ok(CacheEncryptionKey {
                    key: CacheKeyRef {
                        cache: CacheId(k.cache.clone()),
                        id: KeyId(
                            bytes(&k.id)?
                                .try_into()
                                .map_err(|_| ProtocolFailure::InvalidRequest)?,
                        ),
                        purpose: match k.purpose.as_str() {
                            "page" => CacheKeyPurpose::Page,
                            "origin_credentials" => CacheKeyPurpose::OriginCredentials,
                            _ => return Err(ProtocolFailure::InvalidRequest),
                        },
                    },
                    state: match k.state.as_str() {
                        "prepared" => CacheKeyState::Prepared,
                        "active" => CacheKeyState::Active,
                        "retiring" => CacheKeyState::Retiring,
                        _ => return Err(ProtocolFailure::InvalidRequest),
                    },
                    material: bytes(&k.material)?
                        .try_into()
                        .map_err(|_| ProtocolFailure::InvalidRequest)?,
                })
            })
            .collect::<Result<_>>()?,
    };
    validate_bundle(&v)?;
    Ok(v)
}
pub fn encode_bundle(v: &KeyringBundle) -> Result<Vec<u8>> {
    validate_bundle(v)?;
    encode(
        &BundleDto {
            schema_version: v.schema_version,
            cluster: v.cluster.0.clone(),
            generation: v.generation.0.to_string(),
            peer_trust_roots: v
                .peer_trust_roots
                .iter()
                .map(|r| STANDARD.encode(r))
                .collect(),
            cache_keys: v
                .cache_keys
                .iter()
                .map(|k| KeyDto {
                    cache: k.key.cache.0.clone(),
                    id: STANDARD.encode(k.key.id.0),
                    purpose: purpose(k.key.purpose).into(),
                    state: state(k.state).into(),
                    material: STANDARD.encode(k.material),
                })
                .collect(),
        },
        MAX_BUNDLE_BYTES,
    )
}
pub fn decode_error(r: impl Read) -> Result<ErrorResponse> {
    let v = bounded_json(r, MAX_ENROLLMENT_BYTES)?;
    let code = match v.get("code").and_then(Value::as_str) {
        Some("invalid_request") => ProtocolFailure::InvalidRequest,
        Some("unauthenticated") => ProtocolFailure::Unauthenticated,
        Some("forbidden") => ProtocolFailure::Forbidden,
        Some("conflict") => ProtocolFailure::Conflict,
        Some("too_large") => ProtocolFailure::TooLarge,
        Some("unsupported_version") => ProtocolFailure::UnsupportedVersion,
        Some("overloaded") => ProtocolFailure::Overloaded,
        Some("unavailable") => ProtocolFailure::Unavailable,
        _ => return Err(ProtocolFailure::InvalidRequest),
    };
    Ok(ErrorResponse { code })
}
pub fn encode_error(v: &ErrorResponse) -> Result<Vec<u8>> {
    let code = match v.code {
        ProtocolFailure::InvalidRequest => "invalid_request",
        ProtocolFailure::Unauthenticated => "unauthenticated",
        ProtocolFailure::Forbidden => "forbidden",
        ProtocolFailure::Conflict => "conflict",
        ProtocolFailure::TooLarge => "too_large",
        ProtocolFailure::UnsupportedVersion => "unsupported_version",
        ProtocolFailure::Overloaded => "overloaded",
        ProtocolFailure::Unavailable => "unavailable",
    };
    encode(&serde_json::json!({ "code": code }), MAX_ENROLLMENT_BYTES)
}
