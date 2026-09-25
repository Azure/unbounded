//! Exact-name JSON with duplicate rejection, including unknown nested fields.
use super::*;
use crate::{
    error::{Error, Result},
    topology::rails::{RailId, RailMapping},
};
use base64::{Engine, engine::general_purpose::STANDARD};
use serde::{
    Deserialize, Serialize,
    de::{self, DeserializeSeed, MapAccess, SeqAccess, Visitor},
};
use serde_json::Value;
use std::{
    collections::{BTreeMap, HashSet},
    fmt,
    num::NonZeroU32,
};

struct Strict(usize);
impl<'de> DeserializeSeed<'de> for Strict {
    type Value = Value;
    fn deserialize<D: serde::Deserializer<'de>>(
        self,
        d: D,
    ) -> std::result::Result<Value, D::Error> {
        d.deserialize_any(self)
    }
}
impl<'de> Visitor<'de> for Strict {
    type Value = Value;
    fn expecting(&self, f: &mut fmt::Formatter) -> fmt::Result {
        f.write_str("bounded JSON")
    }
    fn visit_bool<E: de::Error>(self, v: bool) -> std::result::Result<Value, E> {
        Ok(Value::Bool(v))
    }
    fn visit_u64<E: de::Error>(self, v: u64) -> std::result::Result<Value, E> {
        Ok(v.into())
    }
    fn visit_i64<E: de::Error>(self, v: i64) -> std::result::Result<Value, E> {
        Ok(v.into())
    }
    fn visit_f64<E: de::Error>(self, v: f64) -> std::result::Result<Value, E> {
        serde_json::Number::from_f64(v)
            .map(Value::Number)
            .ok_or_else(|| E::custom("number"))
    }
    fn visit_str<E: de::Error>(self, v: &str) -> std::result::Result<Value, E> {
        Ok(Value::String(v.into()))
    }
    fn visit_string<E: de::Error>(self, v: String) -> std::result::Result<Value, E> {
        Ok(Value::String(v))
    }
    fn visit_unit<E: de::Error>(self) -> std::result::Result<Value, E> {
        Ok(Value::Null)
    }
    fn visit_seq<A: SeqAccess<'de>>(self, mut a: A) -> std::result::Result<Value, A::Error> {
        if self.0 >= 64 {
            return Err(de::Error::custom("depth"));
        }
        let mut v = Vec::new();
        while let Some(x) = a.next_element_seed(Strict(self.0 + 1))? {
            v.push(x);
        }
        Ok(Value::Array(v))
    }
    fn visit_map<A: MapAccess<'de>>(self, mut a: A) -> std::result::Result<Value, A::Error> {
        if self.0 >= 64 {
            return Err(de::Error::custom("depth"));
        }
        let mut v = serde_json::Map::new();
        while let Some(k) = a.next_key::<String>()? {
            if v.contains_key(&k) {
                return Err(de::Error::custom("duplicate"));
            }
            v.insert(k, a.next_value_seed(Strict(self.0 + 1))?);
        }
        Ok(Value::Object(v))
    }
}
pub(crate) fn strict_json(b: &[u8], limit: usize) -> Result<Value> {
    if b.len() > limit {
        return Err(Error::Overloaded);
    }
    let mut d = serde_json::Deserializer::from_slice(b);
    let v = Strict(0)
        .deserialize(&mut d)
        .map_err(|_| Error::InvalidRequest)?;
    d.end().map_err(|_| Error::InvalidRequest)?;
    Ok(v)
}
fn decode<T: serde::de::DeserializeOwned>(b: &[u8], limit: usize) -> Result<T> {
    serde_json::from_value(strict_json(b, limit)?).map_err(|_| Error::InvalidRequest)
}
fn encode<T: Serialize>(v: &T, limit: usize) -> Result<Vec<u8>> {
    struct Bounded {
        bytes: Vec<u8>,
        limit: usize,
    }
    impl std::io::Write for Bounded {
        fn write(&mut self, b: &[u8]) -> std::io::Result<usize> {
            if b.len() > self.limit - self.bytes.len() {
                return Err(std::io::ErrorKind::FileTooLarge.into());
            }
            self.bytes.extend_from_slice(b);
            Ok(b.len())
        }
        fn flush(&mut self) -> std::io::Result<()> {
            Ok(())
        }
    }
    let mut w = Bounded {
        bytes: Vec::new(),
        limit,
    };
    serde_json::to_writer(&mut w, v).map_err(|_| Error::Overloaded)?;
    // Go's encoder escapes the two JavaScript line separators even with HTML escaping disabled.
    let text = String::from_utf8(w.bytes).map_err(|_| Error::InvalidRequest)?;
    let b = text
        .replace('\u{2028}', "\\u2028")
        .replace('\u{2029}', "\\u2029")
        .into_bytes();
    if b.len() > limit {
        return Err(Error::Overloaded);
    }
    Ok(b)
}
pub fn valid_uuid(s: &str) -> bool {
    s.len() == 36
        && s.bytes().enumerate().all(|(i, b)| {
            if [8, 13, 18, 23].contains(&i) {
                b == b'-'
            } else {
                b.is_ascii_digit() || (b'a'..=b'f').contains(&b)
            }
        })
}
fn uuid(s: &str) -> Result<()> {
    if valid_uuid(s) {
        Ok(())
    } else {
        Err(Error::InvalidRequest)
    }
}
fn header(v: u32, c: &str) -> Result<()> {
    if v != SCHEMA_VERSION {
        return Err(Error::IncompatibleMembership);
    }
    uuid(c)
}
fn counter(s: &str) -> Result<u64> {
    let n = s.parse::<u64>().map_err(|_| Error::InvalidRequest)?;
    if n == 0 || n.to_string() != s {
        return Err(Error::InvalidRequest);
    }
    Ok(n)
}
fn bytes(s: &str) -> Result<Vec<u8>> {
    let b = STANDARD.decode(s).map_err(|_| Error::InvalidRequest)?;
    if STANDARD.encode(&b) != s {
        return Err(Error::InvalidRequest);
    }
    Ok(b)
}
fn certificates(v: &[String]) -> Result<Vec<Vec<u8>>> {
    if v.is_empty() {
        return Err(Error::InvalidRequest);
    }
    v.iter()
        .map(|s| {
            let b = bytes(s)?;
            let (rest, _) =
                x509_parser::parse_x509_certificate(&b).map_err(|_| Error::InvalidRequest)?;
            if !rest.is_empty() {
                return Err(Error::InvalidRequest);
            }
            Ok(b)
        })
        .collect()
}
#[derive(Serialize, Deserialize)]
struct Request {
    schema_version: u32,
    cluster: String,
    enrollment: String,
    csr_der: String,
}
pub fn decode_enrollment_request(b: &[u8]) -> Result<EnrollmentRequest> {
    let r: Request = decode(b, MAX_ENROLLMENT_BYTES)?;
    header(r.schema_version, &r.cluster)?;
    uuid(&r.enrollment)?;
    let csr = bytes(&r.csr_der)?;
    use x509_parser::prelude::FromDer;
    let (rest, _) = x509_parser::certification_request::X509CertificationRequest::from_der(&csr)
        .map_err(|_| Error::InvalidRequest)?;
    if !rest.is_empty() {
        return Err(Error::InvalidRequest);
    }
    Ok(EnrollmentRequest {
        schema_version: r.schema_version,
        cluster: ClusterId(r.cluster),
        enrollment: EnrollmentId(r.enrollment),
        csr_der: csr,
    })
}
pub fn encode_enrollment_request(r: &EnrollmentRequest) -> Result<Vec<u8>> {
    let b = encode(
        &Request {
            schema_version: r.schema_version,
            cluster: r.cluster.0.clone(),
            enrollment: r.enrollment.0.clone(),
            csr_der: STANDARD.encode(&r.csr_der),
        },
        MAX_ENROLLMENT_BYTES,
    )?;
    decode_enrollment_request(&b)?;
    Ok(b)
}
#[derive(Serialize, Deserialize)]
struct Response {
    schema_version: u32,
    cluster: String,
    node: String,
    enrollment: String,
    certificate_chain: Vec<String>,
}
pub fn decode_enrollment_response(b: &[u8]) -> Result<EnrollmentResponse> {
    let r: Response = decode(b, MAX_ENROLLMENT_BYTES)?;
    header(r.schema_version, &r.cluster)?;
    uuid(&r.node)?;
    uuid(&r.enrollment)?;
    let chain = certificates(&r.certificate_chain)?;
    Ok(EnrollmentResponse {
        schema_version: r.schema_version,
        cluster: ClusterId(r.cluster),
        node: NodeId(r.node),
        enrollment: EnrollmentId(r.enrollment),
        certificate_chain: chain,
    })
}
pub fn encode_enrollment_response(r: &EnrollmentResponse) -> Result<Vec<u8>> {
    let b = encode(
        &Response {
            schema_version: r.schema_version,
            cluster: r.cluster.0.clone(),
            node: r.node.0.clone(),
            enrollment: r.enrollment.0.clone(),
            certificate_chain: r
                .certificate_chain
                .iter()
                .map(|c| STANDARD.encode(c))
                .collect(),
        },
        MAX_ENROLLMENT_BYTES,
    )?;
    decode_enrollment_response(&b)?;
    Ok(b)
}
#[derive(Serialize, Deserialize)]
struct Rail {
    rail: u16,
    fabric: String,
    #[serde(
        default,
        skip_serializing_if = "Option::is_none",
        deserialize_with = "nonnull_optional"
    )]
    numa_node: Option<u32>,
}
fn nonnull_optional<'de, D: serde::Deserializer<'de>>(
    d: D,
) -> std::result::Result<Option<u32>, D::Error> {
    u32::deserialize(d).map(Some)
}
#[derive(Serialize, Deserialize)]
struct MemberDto {
    node: String,
    shares: u32,
    peer_endpoint: String,
    rails: Vec<Rail>,
    alignment_enabled: bool,
}
#[derive(Serialize, Deserialize)]
struct Cache {
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
    caches: Vec<Cache>,
}
pub fn decode_publication(b: &[u8]) -> Result<Publication> {
    let value = strict_json(b, MAX_PUBLICATION_BYTES)?;
    if value
        .get("members")
        .and_then(Value::as_array)
        .is_some_and(|a| a.len() > MAX_MEMBERS)
    {
        return Err(Error::Overloaded);
    }
    let p: PublicationDto = serde_json::from_value(value).map_err(|_| Error::InvalidRequest)?;
    header(p.schema_version, &p.cluster)?;
    let sequence = PublicationSequence(counter(&p.sequence)?);
    let membership_version = MembershipVersion(counter(&p.membership_version)?);
    if p.members.len() > MAX_MEMBERS {
        return Err(Error::Overloaded);
    }
    let mut nodes = HashSet::new();
    let mut members = Vec::with_capacity(p.members.len());
    for m in p.members {
        uuid(&m.node)?;
        if !nodes.insert(m.node.clone()) {
            return Err(Error::InvalidRequest);
        }
        let endpoint: std::net::SocketAddr =
            m.peer_endpoint.parse().map_err(|_| Error::InvalidRequest)?;
        if endpoint.port() == 0 || m.peer_endpoint.contains('%') {
            return Err(Error::InvalidRequest);
        }
        let mut ids = HashSet::new();
        let mut rails = Vec::new();
        for r in m.rails {
            if !ids.insert(r.rail) || r.fabric.is_empty() || r.fabric.contains(['\0', '\r', '\n']) {
                return Err(Error::InvalidRequest);
            }
            rails.push(RailMapping {
                rail: RailId(r.rail),
                fabric: r.fabric,
                numa_node: r.numa_node.map(|n| n as usize),
            });
        }
        rails.sort_by_key(|r| r.rail.0);
        members.push(Member {
            node: NodeId(m.node),
            shares: NonZeroU32::new(m.shares).ok_or(Error::InvalidRequest)?,
            peer_endpoint: m.peer_endpoint,
            rails,
            alignment_enabled: m.alignment_enabled,
        });
    }
    members.sort_by(|a, b| a.node.cmp(&b.node));
    let mut caches: Vec<_> = p
        .caches
        .into_iter()
        .map(|c| CacheDefinition {
            id: CacheId(c.id),
            name: c.name,
            client_socket: c.client_socket.into(),
            origin_socket: c.origin_socket.into(),
            socket_mode: c.socket_mode,
        })
        .collect();
    crate::control::caches::validate_definitions(&caches)?;
    caches.sort_by(|a, b| a.id.cmp(&b.id));
    Ok(Publication {
        schema_version: p.schema_version,
        cluster: ClusterId(p.cluster),
        sequence,
        membership_version,
        members,
        caches,
    })
}
fn dto(p: &Publication) -> Result<PublicationDto> {
    let mut members = Vec::new();
    for m in &p.members {
        let mut rails = Vec::new();
        for r in &m.rails {
            rails.push(Rail {
                rail: r.rail.0,
                fabric: r.fabric.clone(),
                numa_node: r
                    .numa_node
                    .map(u32::try_from)
                    .transpose()
                    .map_err(|_| Error::InvalidRequest)?,
            });
        }
        rails.sort_by_key(|r| r.rail);
        members.push(MemberDto {
            node: m.node.0.clone(),
            shares: m.shares.get(),
            peer_endpoint: m.peer_endpoint.clone(),
            rails,
            alignment_enabled: m.alignment_enabled,
        });
    }
    members.sort_by(|a, b| a.node.cmp(&b.node));
    let mut caches = Vec::new();
    for c in &p.caches {
        caches.push(Cache {
            id: c.id.0.clone(),
            name: c.name.clone(),
            client_socket: c
                .client_socket
                .to_str()
                .ok_or(Error::InvalidRequest)?
                .into(),
            origin_socket: c
                .origin_socket
                .to_str()
                .ok_or(Error::InvalidRequest)?
                .into(),
            socket_mode: c.socket_mode,
        });
    }
    caches.sort_by(|a, b| a.id.cmp(&b.id));
    Ok(PublicationDto {
        schema_version: p.schema_version,
        cluster: p.cluster.0.clone(),
        sequence: p.sequence.0.to_string(),
        membership_version: p.membership_version.0.to_string(),
        members,
        caches,
    })
}
pub fn encode_publication(p: &Publication) -> Result<Vec<u8>> {
    if p.members.len() > MAX_MEMBERS {
        return Err(Error::Overloaded);
    }
    let b = encode(&dto(p)?, MAX_PUBLICATION_BYTES)?;
    decode_publication(&b)?;
    Ok(b)
}
pub fn canonical_content(p: &Publication) -> Result<(Vec<u8>, Vec<u8>)> {
    let mut d = dto(p)?;
    d.sequence = "1".into();
    d.membership_version = "1".into();
    decode_publication(&encode(&d, MAX_PUBLICATION_BYTES)?)?;
    #[derive(Serialize)]
    struct Content<'a> {
        schema_version: u32,
        cluster: &'a str,
        members: &'a [MemberDto],
        caches: &'a [Cache],
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
                schema_version: d.schema_version,
                cluster: &d.cluster,
                members: &d.members,
                caches: &d.caches,
            },
            MAX_PUBLICATION_BYTES,
        )?,
        encode(
            &Membership {
                schema_version: d.schema_version,
                cluster: &d.cluster,
                members: &d.members,
            },
            MAX_PUBLICATION_BYTES,
        )?,
    ))
}
#[derive(Serialize, Deserialize)]
struct Bundle {
    schema_version: u32,
    cluster: String,
    generation: String,
    peer_trust_roots: Vec<String>,
    cache_keys: Vec<Key>,
}
#[derive(Serialize, Deserialize)]
struct Key {
    cache: String,
    id: String,
    purpose: String,
    state: String,
    material: String,
}
pub fn decode_bundle(b: &[u8]) -> Result<KeyringBundle> {
    let r: Bundle = decode(b, MAX_BUNDLE_BYTES)?;
    header(r.schema_version, &r.cluster)?;
    let generation = BundleGeneration(counter(&r.generation)?);
    let roots = certificates(&r.peer_trust_roots)?;
    if roots.iter().collect::<HashSet<_>>().len() != roots.len() {
        return Err(Error::InvalidRequest);
    }
    let mut keys = Vec::new();
    let mut seen = HashSet::new();
    let mut active = BTreeMap::new();
    for k in r.cache_keys {
        uuid(&k.cache)?;
        let purpose = match k.purpose.as_str() {
            "page" => CacheKeyPurpose::Page,
            "origin_credentials" => CacheKeyPurpose::OriginCredentials,
            _ => return Err(Error::InvalidRequest),
        };
        let state = match k.state.as_str() {
            "prepared" => CacheKeyState::Prepared,
            "active" => CacheKeyState::Active,
            "retiring" => CacheKeyState::Retiring,
            _ => return Err(Error::InvalidRequest),
        };
        let id: [u8; 16] = bytes(&k.id)?
            .try_into()
            .map_err(|_| Error::InvalidRequest)?;
        let material: [u8; 32] = bytes(&k.material)?
            .try_into()
            .map_err(|_| Error::InvalidRequest)?;
        if !seen.insert((k.cache.clone(), k.purpose.clone(), id)) {
            return Err(Error::InvalidRequest);
        }
        *active.entry((k.cache.clone(), k.purpose)).or_insert(0) +=
            usize::from(state == CacheKeyState::Active);
        keys.push(CacheEncryptionKey {
            key: CacheKeyRef {
                cache: CacheId(k.cache),
                id: KeyId(id),
                purpose,
            },
            state,
            material,
        });
    }
    if active.values().any(|n| *n != 1) {
        return Err(Error::InvalidRequest);
    }
    Ok(KeyringBundle {
        schema_version: r.schema_version,
        cluster: ClusterId(r.cluster),
        generation,
        peer_trust_roots: roots,
        cache_keys: keys,
    })
}
pub fn encode_bundle(b: &KeyringBundle) -> Result<Vec<u8>> {
    let raw = Bundle {
        schema_version: b.schema_version,
        cluster: b.cluster.0.clone(),
        generation: b.generation.0.to_string(),
        peer_trust_roots: b
            .peer_trust_roots
            .iter()
            .map(|r| STANDARD.encode(r))
            .collect(),
        cache_keys: b
            .cache_keys
            .iter()
            .map(|k| Key {
                cache: k.key.cache.0.clone(),
                id: STANDARD.encode(k.key.id.0),
                purpose: match k.key.purpose {
                    CacheKeyPurpose::Page => "page",
                    CacheKeyPurpose::OriginCredentials => "origin_credentials",
                }
                .into(),
                state: match k.state {
                    CacheKeyState::Prepared => "prepared",
                    CacheKeyState::Active => "active",
                    CacheKeyState::Retiring => "retiring",
                }
                .into(),
                material: STANDARD.encode(k.material),
            })
            .collect(),
    };
    let encoded = encode(&raw, MAX_BUNDLE_BYTES)?;
    decode_bundle(&encoded)?;
    Ok(encoded)
}
pub fn decode_error(b: &[u8]) -> Result<ErrorResponse> {
    #[derive(Deserialize)]
    struct Failure {
        code: String,
    }
    let f: Failure = decode(b, MAX_ENROLLMENT_BYTES)?;
    let code = match f.code.as_str() {
        "invalid_request" => ProtocolFailure::InvalidRequest,
        "unauthenticated" => ProtocolFailure::Unauthenticated,
        "forbidden" => ProtocolFailure::Forbidden,
        "conflict" => ProtocolFailure::Conflict,
        "too_large" => ProtocolFailure::TooLarge,
        "unsupported_version" => ProtocolFailure::UnsupportedVersion,
        "overloaded" => ProtocolFailure::Overloaded,
        "unavailable" => ProtocolFailure::Unavailable,
        _ => return Err(Error::InvalidRequest),
    };
    Ok(ErrorResponse { code })
}
pub fn encode_error(response: &ErrorResponse) -> Result<Vec<u8>> {
    #[derive(Serialize)]
    struct Failure<'a> {
        code: &'a str,
    }
    use ProtocolFailure::*;
    let code = match response.code {
        InvalidRequest => "invalid_request",
        Unauthenticated => "unauthenticated",
        Forbidden => "forbidden",
        Conflict => "conflict",
        TooLarge => "too_large",
        UnsupportedVersion => "unsupported_version",
        Overloaded => "overloaded",
        Unavailable => "unavailable",
    };
    encode(&Failure { code }, MAX_ENROLLMENT_BYTES)
}

#[cfg(test)]
mod tests {
    use super::*;
    use sha2::{Digest, Sha256};
    // Copied public vectors from the Go wire package. No private certificate key.
    const PUBLICATION: &str = include_str!("testdata/publication.json");
    const REQUEST: &str = include_str!("testdata/bootstrap-request.json");
    const RESPONSE: &str = include_str!("testdata/bootstrap-response.json");
    const BUNDLE: &str = include_str!("testdata/bundle.json");
    #[test]
    fn go_publication_hashes_and_roundtrips() {
        let p = decode_publication(PUBLICATION.as_bytes()).unwrap();
        let (c, m) = canonical_content(&p).unwrap();
        assert_eq!(
            format!("{:x}", Sha256::digest(c)),
            "9c9791a4a7c4863990f46383126839d3779c5299e5f4426a9487263c2af2e03b"
        );
        assert_eq!(
            format!("{:x}", Sha256::digest(m)),
            "70bcaf18d9a87f3cc72eef79e3163c02c285c186d7811229e5bc2ccd94336617"
        );
        let encoded = encode_publication(&p).unwrap();
        assert_eq!(
            encoded,
            encode_publication(&decode_publication(&encoded).unwrap()).unwrap()
        );
        assert_eq!(
            REQUEST.trim().as_bytes(),
            encode_enrollment_request(&decode_enrollment_request(REQUEST.as_bytes()).unwrap())
                .unwrap()
        );
        assert_eq!(
            RESPONSE.trim().as_bytes(),
            encode_enrollment_response(&decode_enrollment_response(RESPONSE.as_bytes()).unwrap())
                .unwrap()
        );
        assert_eq!(
            BUNDLE.trim().as_bytes(),
            encode_bundle(&decode_bundle(BUNDLE.as_bytes()).unwrap()).unwrap()
        );
    }
    #[test]
    fn strict_numbers_duplicates_unknowns_and_bounds() {
        for (old, new) in [
            (
                "\"schema_version\":1",
                "\"schema_version\":1,\"schema_versi\\u006fn\":1",
            ),
            ("\"future\":{", "\"future\":{\"x\":1,\"x\":2,"),
            ("\"schema_version\":1", "\"schema_version\":2"),
            ("\"rail\":0", "\"rail\":-0"),
            ("\"shares\":4,", "\"shares\":4.0,"),
            ("\"numa_node\":4294967295", "\"numa_node\":null"),
            ("fabric-a", "\\ud800"),
            ("18446744073709551615", "01"),
            ("192.0.2.1:7443", "host:443"),
            ("[2001:db8::1]:7443", "[fe80::1%eth0]:443"),
            (
                "/run/racer/cache-a/client/socket",
                "/run/racer/cache-a//client/socket",
            ),
            ("\"socket_mode\":432", "\"socket_mode\":4095"),
        ] {
            assert!(
                decode_publication(PUBLICATION.replacen(old, new, 1).as_bytes()).is_err(),
                "{new}"
            );
        }
        assert!(
            decode_publication(
                PUBLICATION
                    .replacen(
                        "\"schema_version\":1",
                        "\"schema_version\":1,\"SCHEMA_VERSION\":42",
                        1
                    )
                    .as_bytes()
            )
            .is_ok()
        );
        let mut exact = REQUEST.as_bytes().to_vec();
        exact.resize(MAX_ENROLLMENT_BYTES, b' ');
        assert!(decode_enrollment_request(&exact).is_ok());
        exact.push(b' ');
        assert!(matches!(
            decode_enrollment_request(&exact),
            Err(Error::Overloaded)
        ));
        for bad in [
            "null".to_owned(),
            "[]".into(),
            format!("{REQUEST}{{}}"),
            format!("{}{}", "[".repeat(65), "]".repeat(65)),
        ] {
            assert!(decode_enrollment_request(bad.as_bytes()).is_err());
        }
    }
    #[test]
    fn bundle_rejects_partial_key_epochs() {
        for (old, new) in [
            ("\"state\":\"prepared\"", "\"state\":\"active\""),
            ("\"state\":\"active\"", "\"state\":\"retiring\""),
            ("AAAAAAAAAAAAAAAAAAAAAA==", "AAAAAAAAAAAAAAAAAAAAAB=="),
            ("AAAAAAAAAAAAAAAAAAAAAA==", "AAAAAAAAAAAAAAAAAAAAAA"),
            ("\"purpose\":\"page\"", "\"purpose\":\"future\""),
        ] {
            assert!(decode_bundle(BUNDLE.replacen(old, new, 1).as_bytes()).is_err());
        }
    }
}
