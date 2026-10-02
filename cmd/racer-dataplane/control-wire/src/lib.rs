//! Bounded control DTOs and exact-name JSON with duplicate rejection.
mod definitions;
pub use codec::*;
pub use definitions::*;
use std::time::Duration;

pub const SCHEMA_VERSION: u32 = 1;
pub const BOOTSTRAP_PATH: &str = "/v1/bootstrap";
pub const SNAPSHOT_PATH: &str = "/v1/snapshot";
pub const KEYRING_PATH: &str = "/v1/keyring";
pub const TOKEN_AUDIENCE: &str = "racer-control";
pub const MAX_ENROLLMENT_BYTES: usize = 64 * 1024;
pub const MAX_BUNDLE_BYTES: usize = 512 * 1024;
pub const MAX_PUBLICATION_BYTES: usize = 64 * 1024 * 1024;
pub const MAX_MEMBERS: usize = 100_000;
pub const POLL_WAIT: Duration = Duration::from_secs(30);
pub const RETRY_MIN: Duration = Duration::from_secs(1);
pub const RETRY_MAX: Duration = Duration::from_secs(30);
pub const CERTIFICATE_LIFETIME: Duration = Duration::from_secs(24 * 60 * 60);
pub const RENEW_AFTER: Duration = Duration::from_secs(16 * 60 * 60);
pub const SHARES_ANNOTATION: &str = "racer.unbounded-cloud.io/shares";
pub const RAILS_ANNOTATION: &str = "racer.unbounded-cloud.io/rails";
pub const ALIGNMENT_ANNOTATION: &str = "racer.unbounded-cloud.io/aligned-rails";
pub const EXCLUSION_LABEL: &str = "racer.unbounded-cloud.io/exclude";
pub const DEFAULT_SHARES: u32 = 4;

#[derive(Clone, Copy, Debug, Eq, Ord, PartialEq, PartialOrd)]
pub struct PublicationSequence(pub u64);
#[derive(Clone, Copy, Debug, Eq, Ord, PartialEq, PartialOrd)]
pub struct BundleGeneration(pub u64);
/// Canonical UUID for response correlation, not a durable receipt ID.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct EnrollmentId(pub String);
pub struct SnapshotRequest {
    pub after: Option<PublicationSequence>,
}
pub enum SnapshotResponse {
    Updated(Publication),
    Unchanged,
}
#[derive(Clone)]
pub struct Publication {
    pub schema_version: u32,
    pub cluster: ClusterId,
    pub sequence: PublicationSequence,
    pub membership_version: MembershipVersion,
    pub members: Vec<Member>,
    pub caches: Vec<CacheDefinition>,
}
/// The transport supplies the projected bearer token, never a DTO or diagnostic.
#[derive(Clone)]
pub struct EnrollmentRequest {
    pub shares: u32,
    pub rdma_nics: Vec<RailMapping>,
    pub schema_version: u32,
    pub cluster: ClusterId,
    pub enrollment: EnrollmentId,
    pub csr_der: Vec<u8>,
}
#[derive(Clone)]
pub struct EnrollmentResponse {
    pub schema_version: u32,
    pub cluster: ClusterId,
    pub node: NodeId,
    pub enrollment: EnrollmentId,
    pub certificate_chain: Vec<Vec<u8>>,
}
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum CacheKeyPurpose {
    Page,
    OriginCredentials,
}
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum CacheKeyState {
    Prepared,
    Active,
}
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct CacheKeyRef {
    pub cache: CacheId,
    pub id: KeyId,
    pub purpose: CacheKeyPurpose,
}
/// Deliberately non-Debug; staged key material is wiped on drop.
///
/// ```compile_fail
/// fn diagnostic(key: racer_control_wire::CacheEncryptionKey) {
///     println!("{key:?}");
/// }
/// ```
/// ```compile_fail
/// fn expose(key: racer_control_wire::CacheEncryptionKey) -> [u8; 32] {
///     key.material
/// }
/// ```
/// ```compile_fail
/// fn transfer_twice(key: racer_control_wire::CacheEncryptionKey) {
///     let _first = key.into_installation();
///     let _second = key.into_installation();
/// }
/// ```
#[derive(Clone)]
pub struct CacheEncryptionKey {
    pub key: CacheKeyRef,
    pub state: CacheKeyState,
    material: [u8; 32],
}
impl CacheEncryptionKey {
    /// Accept secret ownership without exposing a borrowed raw-key accessor.
    pub fn new(
        key: CacheKeyRef,
        state: CacheKeyState,
        material: zeroize::Zeroizing<[u8; 32]>,
    ) -> Self {
        Self {
            key,
            state,
            material: *material,
        }
    }

    /// Consume the wire record when transferring to the application's key installer.
    /// Both the source record and returned secret owner wipe their storage on drop.
    pub fn into_installation(self) -> (CacheKeyRef, CacheKeyState, zeroize::Zeroizing<[u8; 32]>) {
        (
            self.key.clone(),
            self.state,
            zeroize::Zeroizing::new(self.material),
        )
    }
}
impl Drop for CacheEncryptionKey {
    fn drop(&mut self) {
        use zeroize::Zeroize;
        self.material.zeroize();
    }
}

#[cfg(test)]
mod transfer_tests {
    use super::*;

    #[test]
    fn consuming_key_transfer_preserves_identity_and_zeroizing_ownership() {
        let reference = CacheKeyRef {
            cache: CacheId("11111111-1111-4111-8111-111111111111".into()),
            id: KeyId::from_generation(1, 7).unwrap(),
            purpose: CacheKeyPurpose::Page,
        };
        let key = CacheEncryptionKey::new(
            reference.clone(),
            CacheKeyState::Active,
            zeroize::Zeroizing::new([0xa7; 32]),
        );
        let (actual, state, material): (_, _, zeroize::Zeroizing<[u8; 32]>) =
            key.into_installation();
        assert_eq!(actual, reference);
        assert_eq!(state, CacheKeyState::Active);
        assert_eq!(*material, [0xa7; 32]);
        assert_eq!(KeyId::from_generation(0, 7), Err(Error::InvalidRequest));
    }
}
/// Missing keys request retirement; no node certificates or private keys.
#[derive(Clone)]
pub struct KeyringBundle {
    pub schema_version: u32,
    pub cluster: ClusterId,
    pub generation: BundleGeneration,
    pub peer_trust_roots: Vec<Vec<u8>>,
    pub cache_keys: Vec<CacheEncryptionKey>,
}
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum ProtocolFailure {
    InvalidRequest,
    Unauthenticated,
    Forbidden,
    Conflict,
    TooLarge,
    UnsupportedVersion,
    Overloaded,
    Unavailable,
}
pub struct ErrorResponse {
    pub code: ProtocolFailure,
}

mod codec {
    use super::*;
    use base64::{Engine, engine::general_purpose::STANDARD};
    use serde::{
        Deserialize, Serialize,
        de::{self, DeserializeSeed, MapAccess, SeqAccess, Visitor},
    };
    use serde_json::Value;
    use std::collections::HashSet;
    use std::{collections::BTreeMap, fmt, num::NonZeroU32};

    #[cfg(test)]
    mod contract_tests {
        include!(concat!(
            env!("CARGO_MANIFEST_DIR"),
            "/src/contract_tests.rs"
        ));
    }

    struct JsonScratch(Value);
    impl Drop for JsonScratch {
        fn drop(&mut self) {
            fn clear(value: &mut Value) {
                use zeroize::Zeroize;
                match value {
                    Value::String(s) => s.zeroize(),
                    Value::Array(values) => values.iter_mut().for_each(clear),
                    Value::Object(values) => values.values_mut().for_each(clear),
                    _ => (),
                }
            }
            clear(&mut self.0);
        }
    }
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
            let mut scratch = JsonScratch(Value::Array(Vec::new()));
            let v = scratch.0.as_array_mut().unwrap();
            while let Some(x) = a.next_element_seed(Strict(self.0 + 1))? {
                v.push(x);
            }
            Ok(std::mem::take(&mut scratch.0))
        }
        fn visit_map<A: MapAccess<'de>>(self, mut a: A) -> std::result::Result<Value, A::Error> {
            if self.0 >= 64 {
                return Err(de::Error::custom("depth"));
            }
            let mut scratch = JsonScratch(Value::Object(serde_json::Map::new()));
            let v = scratch.0.as_object_mut().unwrap();
            while let Some(k) = a.next_key::<String>()? {
                if v.contains_key(&k) {
                    return Err(de::Error::custom("duplicate"));
                }
                v.insert(k, a.next_value_seed(Strict(self.0 + 1))?);
            }
            Ok(std::mem::take(&mut scratch.0))
        }
    }
    pub fn strict_json(b: &[u8], limit: usize) -> Result<Value> {
        if b.len() > limit {
            return Err(Error::Overloaded);
        }
        let mut d = serde_json::Deserializer::from_slice(b);
        let mut v = JsonScratch(
            Strict(0)
                .deserialize(&mut d)
                .map_err(|_| Error::InvalidRequest)?,
        );
        d.end().map_err(|_| Error::InvalidRequest)?;
        Ok(std::mem::take(&mut v.0))
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
    fn key_material(s: &str) -> Result<zeroize::Zeroizing<[u8; 32]>> {
        // Decode directly into a zeroizing owner, including partially written error
        // output. Neither decoding nor canonicality checking allocates secret bytes.
        if s.len() != 44 {
            return Err(Error::InvalidRequest);
        }
        let mut decoded = zeroize::Zeroizing::new([0; 32]);
        let length = STANDARD
            .decode_slice(s, &mut *decoded)
            .map_err(|_| Error::InvalidRequest)?;
        if length != decoded.len() {
            return Err(Error::InvalidRequest);
        }
        let mut canonical = zeroize::Zeroizing::new([0; 44]);
        STANDARD
            .encode_slice(decoded.as_slice(), &mut *canonical)
            .map_err(|_| Error::InvalidRequest)?;
        if canonical.as_slice() != s.as_bytes() {
            return Err(Error::InvalidRequest);
        }
        Ok(decoded)
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
    #[serde(deny_unknown_fields)]
    struct Request {
        shares: u32,
        rdma_nics: Vec<Rail>,
        schema_version: u32,
        cluster: String,
        enrollment: String,
        csr_der: String,
    }
    pub fn decode_enrollment_request(b: &[u8]) -> Result<EnrollmentRequest> {
        let r: Request = decode(b, MAX_ENROLLMENT_BYTES)?;
        if r.shares == 0 {
            return Err(Error::InvalidRequest);
        }
        header(r.schema_version, &r.cluster)?;
        uuid(&r.enrollment)?;
        let csr = bytes(&r.csr_der)?;
        use x509_parser::prelude::FromDer;
        let (rest, _) =
            x509_parser::certification_request::X509CertificationRequest::from_der(&csr)
                .map_err(|_| Error::InvalidRequest)?;
        if !rest.is_empty() {
            return Err(Error::InvalidRequest);
        }
        Ok(EnrollmentRequest {
            shares: r.shares,
            rdma_nics: nics_from_dto(r.rdma_nics)?,
            schema_version: r.schema_version,
            cluster: ClusterId(r.cluster),
            enrollment: EnrollmentId(r.enrollment),
            csr_der: csr,
        })
    }
    pub fn encode_enrollment_request(r: &EnrollmentRequest) -> Result<Vec<u8>> {
        let b = encode(
            &Request {
                shares: r.shares,
                rdma_nics: nics_to_dto(&r.rdma_nics)?,
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
    #[serde(deny_unknown_fields)]
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
    #[derive(Clone, Serialize, Deserialize)]
    #[serde(deny_unknown_fields)]
    struct Rail {
        device: String,
        port: u8,
        rail: u16,
        #[serde(
            default,
            skip_serializing_if = "Option::is_none",
            deserialize_with = "nonnull_gid"
        )]
        gid: Option<String>,
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
    fn nonnull_gid<'de, D: serde::Deserializer<'de>>(
        d: D,
    ) -> std::result::Result<Option<String>, D::Error> {
        String::deserialize(d).map(Some)
    }
    fn nics_from_dto(nics: Vec<Rail>) -> Result<Vec<RailMapping>> {
        if nics.len() > 64 {
            return Err(Error::Overloaded);
        }
        let mut physical = HashSet::new();
        let mut result = Vec::new();
        for nic in nics {
            if nic.device.is_empty()
                || nic.device.contains(['\0', '\r', '\n'])
                || nic.port == 0
                || !physical.insert((nic.device.clone(), nic.port))
            {
                return Err(Error::InvalidRequest);
            }
            let gid = nic
                .gid
                .map(|value| {
                    if value.len() != 32
                        || !value
                            .bytes()
                            .all(|b| b.is_ascii_digit() || matches!(b, b'a'..=b'f'))
                    {
                        return Err(Error::InvalidRequest);
                    }
                    let mut gid = [0; 16];
                    for (i, byte) in gid.iter_mut().enumerate() {
                        *byte = u8::from_str_radix(&value[i * 2..i * 2 + 2], 16)
                            .map_err(|_| Error::InvalidRequest)?;
                    }
                    Ok(gid)
                })
                .transpose()?;
            result.push(RailMapping {
                device: nic.device,
                port: nic.port,
                rail: RailId(nic.rail),
                gid,
                numa_node: nic.numa_node.map(|n| n as usize),
            });
        }
        result.sort_by(|a, b| (a.rail, &a.device, a.port).cmp(&(b.rail, &b.device, b.port)));
        Ok(result)
    }
    fn nics_to_dto(nics: &[RailMapping]) -> Result<Vec<Rail>> {
        let mut result = nics
            .iter()
            .map(|nic| {
                Ok(Rail {
                    device: nic.device.clone(),
                    port: nic.port,
                    rail: nic.rail.0,
                    gid: nic
                        .gid
                        .map(|gid| gid.iter().map(|b| format!("{b:02x}")).collect()),
                    numa_node: nic
                        .numa_node
                        .map(u32::try_from)
                        .transpose()
                        .map_err(|_| Error::InvalidRequest)?,
                })
            })
            .collect::<Result<Vec<_>>>()?;
        result.sort_by(|a, b| (a.rail, &a.device, a.port).cmp(&(b.rail, &b.device, b.port)));
        Ok(result)
    }
    #[derive(Clone, Serialize, Deserialize)]
    #[serde(deny_unknown_fields)]
    struct MemberDto {
        node: String,
        shares: u32,
        peer_endpoint: String,
        rdma_nics: Vec<Rail>,
        site: String,
    }
    #[derive(Clone, Serialize, Deserialize)]
    #[serde(deny_unknown_fields)]
    struct Cache {
        id: String,
        name: String,
        client_socket: String,
        origin_socket: String,
    }
    #[derive(Clone, Serialize, Deserialize)]
    #[serde(deny_unknown_fields)]
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
        publication_from_dto(p)
    }

    /// Validate both decoded wire publications and direct in-process candidates.
    fn publication_from_dto(p: PublicationDto) -> Result<Publication> {
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
            if endpoint.port() == 0 || m.peer_endpoint.contains('%') || !valid_site(&m.site) {
                return Err(Error::InvalidRequest);
            }
            let rails = nics_from_dto(m.rdma_nics)?;
            members.push(Member {
                node: NodeId(m.node),
                shares: NonZeroU32::new(m.shares).ok_or(Error::InvalidRequest)?,
                peer_endpoint: m.peer_endpoint,
                rails,
                site: m.site,
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
            })
            .collect();
        validate_definitions(&caches)?;
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
            let rdma_nics = nics_to_dto(&m.rails)?;
            members.push(MemberDto {
                node: m.node.0.clone(),
                shares: m.shares.get(),
                peer_endpoint: m.peer_endpoint.clone(),
                rdma_nics,
                site: m.site.clone(),
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
        let d = dto(p)?;
        let b = encode(&d, MAX_PUBLICATION_BYTES)?;
        publication_from_dto(d)?;
        Ok(b)
    }

    /// Validate and normalize a local publication without decoding JSON again.
    pub fn validate_publication(p: &Publication) -> Result<Publication> {
        if p.members.len() > MAX_MEMBERS {
            return Err(Error::Overloaded);
        }
        let d = dto(p)?;
        // Preserve the wire byte bound even for callers that bypass decoding.
        encode(&d, MAX_PUBLICATION_BYTES)?;
        publication_from_dto(d)
    }
    pub fn canonical_content(p: &Publication) -> Result<(Vec<u8>, Vec<u8>)> {
        let mut d = dto(p)?;
        d.sequence = "1".into();
        d.membership_version = "1".into();
        encode(&d, MAX_PUBLICATION_BYTES)?;
        publication_from_dto(d.clone())?;
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

    #[derive(Deserialize)]
    #[serde(deny_unknown_fields)]
    struct DeltaDto {
        delta_version: u32,
        cluster: String,
        base_sequence: String,
        base_hash: String,
        sequence: String,
        membership_version: String,
        content_hash: String,
        upsert_members: Vec<MemberDto>,
        remove_members: Vec<String>,
        caches: Vec<Cache>,
    }

    pub fn content_hash(p: &Publication) -> Result<String> {
        use sha2::Digest;
        let (content, _) = canonical_content(p)?;
        Ok(format!("{:x}", sha2::Sha256::digest(content)))
    }

    /// Apply only against the exact authenticated base. Caller requests a full image
    /// on any delta failure; no partially applied state is ever published.
    pub fn apply_delta(base: &Publication, bytes: &[u8]) -> Result<Publication> {
        let d: DeltaDto = decode(bytes, 4 * 1024 * 1024)?;
        if d.delta_version != 1
            || d.cluster != base.cluster.0
            || counter(&d.base_sequence)? != base.sequence.0
            || d.base_hash != content_hash(base)?
            || counter(&d.sequence)? <= base.sequence.0
            || counter(&d.membership_version)? < base.membership_version.0
        {
            return Err(Error::Replay);
        }
        let mut original = dto(base)?;
        let mut members: BTreeMap<_, _> = original
            .members
            .into_iter()
            .map(|m| (m.node.clone(), m))
            .collect();
        let mut seen = HashSet::new();
        for id in d.remove_members {
            if !seen.insert(id.clone()) || members.remove(&id).is_none() {
                return Err(Error::InvalidRequest);
            }
        }
        for member in d.upsert_members {
            if !seen.insert(member.node.clone()) {
                return Err(Error::InvalidRequest);
            }
            members.insert(member.node.clone(), member);
        }
        if members.len() > MAX_MEMBERS {
            return Err(Error::Overloaded);
        }
        original.members = members.into_values().collect();
        original.caches = d.caches;
        original.sequence = d.sequence;
        original.membership_version = d.membership_version;
        encode(&original, MAX_PUBLICATION_BYTES)?;
        let next = publication_from_dto(original)?;
        if content_hash(&next)? != d.content_hash {
            return Err(Error::Replay);
        }
        Ok(next)
    }

    #[cfg(test)]
    mod delta_tests {
        use super::*;
        #[test]
        fn site_only_deltas_add_change_remove_and_reject_tampering() {
            let mut base = decode_publication(include_bytes!(concat!(
                env!("CARGO_MANIFEST_DIR"),
                "/../../../internal/racer/wire/testdata/publication.json"
            )))
            .unwrap();
            base.sequence.0 = 1;
            base.membership_version.0 = 1;
            for site in ["Site_1.a-b", "site2", ""] {
                let mut next = base.clone();
                next.sequence.0 += 1;
                next.membership_version.0 += 1;
                next.members[0].site = site.into();
                let dto = dto(&next).unwrap();
                let mut delta = serde_json::json!({
                    "delta_version":1,"cluster":base.cluster.0,"base_sequence":base.sequence.0.to_string(),
                    "base_hash":content_hash(&base).unwrap(),"sequence":next.sequence.0.to_string(),
                    "membership_version":next.membership_version.0.to_string(),"content_hash":content_hash(&next).unwrap(),
                    "upsert_members":[dto.members[0]],"remove_members":[],"caches":dto.caches
                });
                let accepted = apply_delta(&base, &serde_json::to_vec(&delta).unwrap()).unwrap();
                assert_eq!(accepted.members[0].site, site);
                assert_eq!(
                    encode_publication(&accepted).unwrap(),
                    encode_publication(&next).unwrap()
                );
                for bad in ["other", "-invalid"] {
                    delta["upsert_members"][0]["site"] = bad.into();
                    assert!(apply_delta(&base, &serde_json::to_vec(&delta).unwrap()).is_err());
                }
                base = accepted;
            }
        }

        #[test]
        fn delta_add_remove_update_hash_and_replay() {
            let mut base = decode_publication(include_bytes!(concat!(
                env!("CARGO_MANIFEST_DIR"),
                "/../../../internal/racer/wire/testdata/publication.json"
            )))
            .unwrap();
            base.sequence.0 = 1;
            base.membership_version.0 = 1;
            let mut next = base.clone();
            next.sequence.0 += 1;
            next.membership_version.0 += 1;
            next.members[0].shares = NonZeroU32::new(9).unwrap();
            let removed = next.members.pop().unwrap().node.0;
            let mut added = next.members[0].clone();
            added.node = NodeId("77777777-7777-4777-8777-777777777777".into());
            next.members.push(added);
            let dto = dto(&next).unwrap();
            let mut delta = serde_json::json!({
                "delta_version":1,"cluster":base.cluster.0,"base_sequence":base.sequence.0.to_string(),
                "base_hash":content_hash(&base).unwrap(),"sequence":next.sequence.0.to_string(),
                "membership_version":next.membership_version.0.to_string(),"content_hash":content_hash(&next).unwrap(),
                "upsert_members":dto.members,"remove_members":[removed],"caches":dto.caches
            });
            let bytes = serde_json::to_vec(&delta).unwrap();
            assert_eq!(
                content_hash(&apply_delta(&base, &bytes).unwrap()).unwrap(),
                content_hash(&next).unwrap()
            );
            assert!(apply_delta(&next, &bytes).is_err());
            delta["content_hash"] = "00".repeat(32).into();
            assert!(apply_delta(&base, &serde_json::to_vec(&delta).unwrap()).is_err());
        }
    }
    #[derive(Serialize, Deserialize)]
    #[serde(deny_unknown_fields)]
    struct Bundle {
        schema_version: u32,
        cluster: String,
        generation: String,
        peer_trust_roots: Vec<String>,
        cache_keys: Vec<Key>,
    }
    #[derive(Serialize, Deserialize)]
    #[serde(deny_unknown_fields)]
    struct Key {
        cache: String,
        id: String,
        purpose: String,
        state: String,
        material: String,
    }
    impl Drop for Key {
        fn drop(&mut self) {
            use zeroize::Zeroize;
            self.material.zeroize();
        }
    }
    pub fn decode_bundle(b: &[u8]) -> Result<KeyringBundle> {
        let scratch = JsonScratch(strict_json(b, MAX_BUNDLE_BYTES)?);
        let r = Bundle::deserialize(&scratch.0).map_err(|_| Error::InvalidRequest)?;
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
                _ => return Err(Error::InvalidRequest),
            };
            let id: [u8; 16] = bytes(&k.id)?
                .try_into()
                .map_err(|_| Error::InvalidRequest)?;
            let created = u64::from_be_bytes(id[4..12].try_into().unwrap());
            if &id[..4] != b"RKG1" || created == 0 || created > generation.0 {
                return Err(Error::InvalidRequest);
            }
            let material = key_material(&k.material)?;
            if !seen.insert((k.cache.clone(), k.purpose.clone(), id)) {
                return Err(Error::InvalidRequest);
            }
            *active
                .entry((k.cache.clone(), k.purpose.clone()))
                .or_insert(0) += usize::from(state == CacheKeyState::Active);
            keys.push(CacheEncryptionKey {
                key: CacheKeyRef {
                    cache: CacheId(k.cache.clone()),
                    id: KeyId(id),
                    purpose,
                },
                state,
                material: *material,
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
                    }
                    .into(),
                    material: STANDARD.encode(k.material),
                })
                .collect(),
        };
        struct BundleOutput(zeroize::Zeroizing<Vec<u8>>);
        impl std::io::Write for BundleOutput {
            fn write(&mut self, bytes: &[u8]) -> std::io::Result<usize> {
                if bytes.len() > MAX_BUNDLE_BYTES - self.0.len() {
                    return Err(std::io::ErrorKind::FileTooLarge.into());
                }
                self.0.extend_from_slice(bytes);
                Ok(bytes.len())
            }
            fn flush(&mut self) -> std::io::Result<()> {
                Ok(())
            }
        }
        // Fixed maximum capacity prevents reallocating and freeing a partially
        // serialized secret. All bundle fields are ASCII after validation.
        let mut encoded = BundleOutput(zeroize::Zeroizing::new(Vec::with_capacity(
            MAX_BUNDLE_BYTES,
        )));
        serde_json::to_writer(&mut encoded, &raw).map_err(|_| Error::Overloaded)?;
        decode_bundle(&encoded.0)?;
        Ok(std::mem::take(&mut *encoded.0))
    }
    pub fn decode_error(b: &[u8]) -> Result<ErrorResponse> {
        #[derive(Deserialize)]
        #[serde(deny_unknown_fields)]
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
        // Shared public vectors from the Go wire package. No private certificate key.
        const PUBLICATION: &str = include_str!(concat!(
            env!("CARGO_MANIFEST_DIR"),
            "/../../../internal/racer/wire/testdata/publication.json"
        ));
        const REQUEST: &str = include_str!(concat!(
            env!("CARGO_MANIFEST_DIR"),
            "/../../../internal/racer/wire/testdata/bootstrap-request.json"
        ));
        const RESPONSE: &str = include_str!(concat!(
            env!("CARGO_MANIFEST_DIR"),
            "/../../../internal/racer/wire/testdata/bootstrap-response.json"
        ));
        const BUNDLE: &str = include_str!(concat!(
            env!("CARGO_MANIFEST_DIR"),
            "/../../../internal/racer/wire/testdata/bundle.json"
        ));
        #[test]
        fn key_material_requires_exact_length_canonical_padding_and_trailing_bits() {
            let canonical = STANDARD.encode([0xa7; 32]);
            assert_eq!(*key_material(&canonical).unwrap(), [0xa7; 32]);
            let mut trailing_bits = canonical.clone().into_bytes();
            assert_eq!(trailing_bits[42], b'c');
            trailing_bits[42] = b'd';
            let mut invalid_tail = canonical.clone().into_bytes();
            invalid_tail[40] = b'!';
            let mut embedded_padding = canonical.clone().into_bytes();
            embedded_padding[20] = b'=';
            for malformed in [
                canonical.trim_end_matches('=').to_owned(),
                format!("{canonical}="),
                format!("{canonical}\n"),
                canonical.replace('=', " "),
                String::from_utf8(trailing_bits).unwrap(),
                String::from_utf8(invalid_tail).unwrap(),
                String::from_utf8(embedded_padding).unwrap(),
                STANDARD.encode([0xa7; 31]),
                STANDARD.encode([0xa7; 33]),
                String::new(),
            ] {
                assert!(matches!(
                    key_material(&malformed),
                    Err(Error::InvalidRequest)
                ));
                let mut bundle: Value = serde_json::from_str(BUNDLE).unwrap();
                bundle["cache_keys"][0]["material"] = malformed.into();
                assert!(matches!(
                    decode_bundle(&serde_json::to_vec(&bundle).unwrap()),
                    Err(Error::InvalidRequest)
                ));
            }
        }
        #[test]
        fn go_delta_vector_applies_exactly_and_rejects_tampering() {
            let base = decode_publication(br#"{"schema_version":1,"cluster":"11111111-1111-4111-8111-111111111111","sequence":"1","membership_version":"1","members":[{"node":"22222222-2222-4222-8222-222222222222","shares":4,"peer_endpoint":"127.0.0.1:7443","rdma_nics":[],"site":""},{"node":"33333333-3333-4333-8333-333333333333","shares":4,"peer_endpoint":"127.0.0.2:7443","rdma_nics":[],"site":""}],"caches":[]}"#).unwrap();
            let delta = include_str!(concat!(
                env!("CARGO_MANIFEST_DIR"),
                "/../../../internal/racer/wire/testdata/delta.json"
            ));
            let next = apply_delta(&base, delta.as_bytes()).unwrap();
            assert_eq!(next.sequence.0, 2);
            assert_eq!(next.members.len(), 2);
            assert_eq!(next.members[0].shares.get(), 9);
            assert_eq!(next.members[1].shares.get(), 7);
            assert_eq!(
                format!("{:x}", Sha256::digest(canonical_content(&next).unwrap().0)),
                "0929db4d1b89c4fdb5c6c20d938a77b286318a830df0b1ac823ee62916af4e20"
            );
            assert!(apply_delta(&next, delta.as_bytes()).is_err());
            assert!(
                apply_delta(
                    &base,
                    delta.replace("\"shares\":9", "\"shares\":8").as_bytes()
                )
                .is_err()
            );
        }
        #[test]
        fn go_publication_hashes_and_roundtrips() {
            let p = decode_publication(PUBLICATION.as_bytes()).unwrap();
            let (c, m) = canonical_content(&p).unwrap();
            assert_eq!(
                format!("{:x}", Sha256::digest(c)),
                "1f68b286bce90f488529367057850804d24be4dc5e81c811cc06ebfa376c0a0e"
            );
            assert_eq!(
                format!("{:x}", Sha256::digest(m)),
                "c523f9a1b8f503628a8dbcef9fa0ef35553640b7c226515f5f5d70a9f3abe0ac"
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
                encode_enrollment_response(
                    &decode_enrollment_response(RESPONSE.as_bytes()).unwrap()
                )
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
                (
                    "\"schema_version\":1",
                    "\"schema_version\":1,\"future\":{\"x\":1,\"x\":2}",
                ),
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
                .is_err()
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
                ("UktHMQAAAAAAAAABAAAAAA==", "UktHMQAAAAAAAAABAAAAAB=="),
                ("UktHMQAAAAAAAAABAAAAAA==", "UktHMQAAAAAAAAABAAAAAA"),
                ("\"purpose\":\"page\"", "\"purpose\":\"future\""),
            ] {
                assert!(decode_bundle(BUNDLE.replacen(old, new, 1).as_bytes()).is_err());
            }
        }
    }
}
