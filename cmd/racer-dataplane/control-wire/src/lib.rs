//! Bounded control records and canonical JSON for Racer enrollment and publication.
//!
//! This crate owns identities, publications, deltas, keyring records, duplicate
//! rejection, and syntax validation. It does not depend on an executor, transport,
//! placement algorithm, or application. Callers own installation, persistence,
//! accepted control cursors, and mapping payload-free failures to their errors.
//!
//! Key bundles transfer into `racer_crypto::identity` for certificate validation
//! and purpose-bound leases. Wire key material is private and deliberately
//! non-Debug. [`CacheEncryptionKey::into_installation`] returns a zeroizing secret
//! owner; the source record also wipes on drop. Encoded bundles still contain
//! secrets, so callers must protect the returned bytes. Key IDs reject zero
//! generations as a syntax error; identity independently validates epoch policy.
//!
//! # Enrollment and application ownership
//!
//! `racer_crypto::enrollment` owns durable node-private enrollment, certificate
//! validation, projected token reads, and identity recovery. It uses
//! `uring_runtime::reactor::filesystem::secure` for bounded descriptor-relative I/O,
//! private access checks, durable replacement, and fencing abandoned attempts.
//! The caller supplies current NIC inventory and shares, persists NIC reservations
//! before preparing enrollment, and exclusively owns the identity directory.
//! `racer_crypto::identity::BundleInstaller` owns canonical bundle replay tracking
//! and installation into its keyring; `wire_codec::rest` owns reusable REST/TLS.
//!
//! In the full application, publication adapters bind these wire records to the
//! separate `controlplane` component's synchronization, immutable publication,
//! retention, and generation-tagged rollout APIs. That component is application
//! context, not a dependency or member of this extracted workspace. Application
//! adapters own placement, cache/resource installation, credential policy, and
//! error classification. Only owned preparation jobs and immutable shared
//! generations cross threads, not worker-local I/O or credentials. Topology
//! projection adapters remain caller-owned under Rust's orphan rules; rail
//! mappings and cache definitions pass through without conversion.
//!
//! Tests consume Go-owned fixtures in `internal/racer/wire/testdata`; Rust does
//! not regenerate them. Run from the repository root:
//!
//! ```sh
//! timeout --signal=TERM --kill-after=10s 300s cargo test --locked --manifest-path cmd/racer-dataplane/Cargo.toml -p racer-control-wire
//! ```

pub use codec::*;
pub use definitions::*;
use std::time::Duration;

/// Current control record schema version.
pub const SCHEMA_VERSION: u32 = 1;

/// Enrollment endpoint for a new node identity.
pub const BOOTSTRAP_PATH: &str = "/v1/bootstrap";

/// Full or incremental publication endpoint.
pub const SNAPSHOT_PATH: &str = "/v1/snapshot";

/// Key epoch distribution endpoint.
pub const KEYRING_PATH: &str = "/v1/keyring";

/// Projected bearer token audience used by control callers.
pub const TOKEN_AUDIENCE: &str = "racer-control";

/// Maximum encoded enrollment request or response size.
pub const MAX_ENROLLMENT_BYTES: usize = 64 * 1024;

/// Maximum encoded keyring bundle size.
pub const MAX_BUNDLE_BYTES: usize = 512 * 1024;

/// Maximum encoded publication size.
pub const MAX_PUBLICATION_BYTES: usize = 64 * 1024 * 1024;

/// Maximum members accepted in one publication.
pub const MAX_MEMBERS: usize = 100_000;

/// Recommended long-poll wait interval.
pub const POLL_WAIT: Duration = Duration::from_secs(30);

/// Lower bound for caller retry backoff.
pub const RETRY_MIN: Duration = Duration::from_secs(1);

/// Upper bound for caller retry backoff.
pub const RETRY_MAX: Duration = Duration::from_secs(30);

/// Requested lifetime for an enrolled node certificate.
pub const CERTIFICATE_LIFETIME: Duration = Duration::from_secs(24 * 60 * 60);

/// Recommended age at which callers renew node certificates.
pub const RENEW_AFTER: Duration = Duration::from_secs(16 * 60 * 60);

/// Default member weight for local enrollment requests.
pub const DEFAULT_SHARES: u32 = 4;

/// Monotonic publication cursor within a cluster.
#[derive(Clone, Copy, Debug, Eq, Ord, PartialEq, PartialOrd)]
pub struct PublicationSequence(pub u64);

/// Monotonic generation identifying a complete keyring bundle.
#[derive(Clone, Copy, Debug, Eq, Ord, PartialEq, PartialOrd)]
pub struct BundleGeneration(pub u64);

/// Canonical UUID for response correlation, not a durable receipt ID.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct EnrollmentId(pub String);

/// Request a publication newer than an optional accepted cursor.
pub struct SnapshotRequest {
    pub after: Option<PublicationSequence>,
}

/// A newer publication or confirmation that the current cursor is unchanged.
pub enum SnapshotResponse {
    Updated(Publication),

    Unchanged,
}

/// Complete membership and cache configuration at one publication cursor.
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

/// Assigned node identity and certificate chain correlated to an enrollment.
#[derive(Clone)]
pub struct EnrollmentResponse {
    pub block_devices: Option<String>,

    pub schema_version: u32,

    pub cluster: ClusterId,

    pub node: NodeId,

    pub enrollment: EnrollmentId,

    pub certificate_chain: Vec<Vec<u8>>,
}

/// Immutable use assigned to a cache encryption key.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum CacheKeyPurpose {
    Page,

    OriginCredentials,
}

/// Whether a key is staged for future use or currently active.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum CacheKeyState {
    Prepared,

    Active,
}

/// Cache, epoch ID, and purpose bound to one secret.
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

    /// Consume the wire record when transferring to the identity component's installer.
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
    /// Wipe the source secret even after transferring a zeroizing copy.
    fn drop(&mut self) {
        use zeroize::Zeroize;
        self.material.zeroize();
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

/// Stable error codes exchanged with control callers.
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

/// Payload-free control error response.
pub struct ErrorResponse {
    pub code: ProtocolFailure,
}

/// Pure identities, records, and syntax validation without runtime policy.
mod definitions {
    use std::{collections::HashSet, num::NonZeroU32, path::PathBuf};

    /// A validated wire value or a payload-free contract failure.
    pub type Result<T> = std::result::Result<T, Error>;

    /// Payload-free failures. Applications map these into their own error domain.
    #[derive(Clone, Copy, Debug, Eq, PartialEq)]
    pub enum Error {
        InvalidRequest,

        IncompatibleMembership,

        Overloaded,

        Replay,
    }

    impl std::fmt::Display for Error {
        /// Describe only the error category, never the rejected input.
        fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
            write!(f, "{self:?}")
        }
    }

    impl std::error::Error for Error {}

    /// Cluster UUID binding every control record to its authority.
    #[derive(Clone, Debug, Eq, Hash, Ord, PartialEq, PartialOrd)]
    pub struct ClusterId(pub String);

    /// Kubernetes ClusterCache UID, not its reusable resource name.
    #[derive(Clone, Debug, Eq, Hash, Ord, PartialEq, PartialOrd)]
    pub struct CacheId(pub String);

    /// Kubernetes Node UID, not its reusable resource name.
    #[derive(Clone, Debug, Eq, Hash, Ord, PartialEq, PartialOrd)]
    pub struct NodeId(pub String);

    /// Version of the membership portion of a publication.
    #[derive(Clone, Copy, Debug, Eq, Hash, PartialEq)]
    pub struct MembershipVersion(pub u64);

    /// Opaque key identity encoding its nonzero creation generation.
    #[derive(Clone, Copy, Debug, Eq, Hash, PartialEq)]
    pub struct KeyId(pub [u8; 16]);

    /// Logical network rail shared by one or more physical NIC ports.
    #[derive(Clone, Copy, Debug, Eq, Ord, PartialEq, PartialOrd)]
    pub struct RailId(pub u16);

    /// Physical NIC port and its optional address and NUMA locality.
    #[derive(Clone, Debug, Eq, PartialEq)]
    pub struct RailMapping {
        pub rail: RailId,

        pub device: String,

        pub port: u8,

        pub gid: Option<[u8; 16]>,

        pub numa_node: Option<usize>,
    }

    /// Wire membership record, deliberately not a placement algorithm input trait.
    #[derive(Clone, Debug)]
    pub struct Member {
        pub node: NodeId,

        pub shares: NonZeroU32,

        pub peer_endpoint: String,

        pub rails: Vec<RailMapping>,

        pub site: String,
    }

    /// Named cache and its canonical client and origin Unix sockets.
    #[derive(Clone, Debug, Eq, PartialEq)]
    pub struct CacheDefinition {
        pub id: CacheId,

        pub name: String,

        pub client_socket: PathBuf,

        pub origin_socket: PathBuf,
    }

    impl KeyId {
        /// Construct an epoch-bound ID with a controller-selected uniqueness suffix.
        pub fn from_generation(generation: u64, suffix: u32) -> Result<Self> {
            if generation == 0 {
                return Err(Error::InvalidRequest);
            }
            let mut bytes = [0; 16];
            bytes[..4].copy_from_slice(b"RKG1");
            bytes[4..12].copy_from_slice(&generation.to_be_bytes());
            bytes[12..].copy_from_slice(&suffix.to_be_bytes());
            Ok(Self(bytes))
        }

        /// Read the nonzero creation generation of an RKG1 ID, not bundle admission policy.
        pub fn generation(self) -> Option<u64> {
            (self.0[..4] == *b"RKG1")
                .then(|| u64::from_be_bytes(self.0[4..12].try_into().expect("generation bytes")))
                .filter(|generation| *generation != 0)
        }
    }

    /// Canonical lowercase UUID syntax only; callers choose nil/version policy.
    pub fn valid_uuid(value: &str) -> bool {
        value.len() == 36
            && value.bytes().enumerate().all(|(i, b)| {
                if matches!(i, 8 | 13 | 18 | 23) {
                    b == b'-'
                } else {
                    b.is_ascii_digit() || (b'a'..=b'f').contains(&b)
                }
            })
    }

    /// Accept an absent site or a bounded ASCII site label.
    pub fn valid_site(value: &str) -> bool {
        value.is_empty()
            || (value.len() <= 63
                && value.as_bytes()[0].is_ascii_alphanumeric()
                && value.as_bytes()[value.len() - 1].is_ascii_alphanumeric()
                && value
                    .bytes()
                    .all(|b| b.is_ascii_alphanumeric() || matches!(b, b'-' | b'_' | b'.')))
    }

    /// Validate a cache name and derive paths that fit Unix socket address limits.
    pub fn canonical_socket_paths(name: &str) -> Result<(PathBuf, PathBuf)> {
        if name.is_empty()
            || name.len() > 253
            || !name.split('.').all(|label| {
                !label.is_empty()
                    && label.len() <= 63
                    && label.bytes().enumerate().all(|(i, b)| {
                        b.is_ascii_lowercase()
                            || b.is_ascii_digit()
                            || b == b'-' && i != 0 && i + 1 != label.len()
                    })
            })
        {
            return Err(Error::InvalidRequest);
        }
        let client = format!("/run/racer/{name}/client/socket");
        let origin = format!("/run/racer/{name}/origin/socket");
        if client.len() > 107 || origin.len() > 107 {
            return Err(Error::InvalidRequest);
        }
        Ok((client.into(), origin.into()))
    }

    /// Reject duplicate cache identities and paths that differ from canonical spelling.
    pub fn validate_definitions(definitions: &[CacheDefinition]) -> Result<()> {
        let mut ids = HashSet::new();
        let mut names = HashSet::new();
        for d in definitions {
            if !crate::valid_uuid(&d.id.0) || !ids.insert(&d.id) || !names.insert(&d.name) {
                return Err(Error::InvalidRequest);
            }
            let (client, origin) = canonical_socket_paths(&d.name)?;
            // Path equality normalizes separators; the wire requires exact strings.
            if d.client_socket.as_os_str() != client.as_os_str()
                || d.origin_socket.as_os_str() != origin.as_os_str()
            {
                return Err(Error::InvalidRequest);
            }
        }
        Ok(())
    }

    /// Syntax boundaries remain independent of application admission policy.
    #[cfg(test)]
    mod tests {
        use super::*;

        /// Key IDs require their magic and epoch, but leave suffix selection to callers.
        #[test]
        fn key_generation_requires_magic_and_nonzero_epoch_but_not_a_suffix_policy() {
            assert_eq!(KeyId::from_generation(0, 0), Err(Error::InvalidRequest));
            for generation in [1, 256, u64::MAX] {
                for suffix in [0, u32::MAX] {
                    let id = KeyId::from_generation(generation, suffix).unwrap();
                    assert_eq!(id.generation(), Some(generation));
                    let mut invalid = id;
                    invalid.0[0] ^= 1;
                    assert_eq!(invalid.generation(), None);
                    invalid = id;
                    invalid.0[4..12].fill(0);
                    assert_eq!(invalid.generation(), None);
                }
            }
            assert_eq!(KeyId([0; 16]).generation(), None);
        }

        /// UUID spelling rejects aliases while accepting nil and arbitrary versions.
        #[test]
        fn uuid_syntax_rejects_aliases_without_enforcing_version_or_nil_policy() {
            for value in [
                "00000000-0000-0000-0000-000000000000",
                "ffffffff-ffff-ffff-ffff-ffffffffffff",
                "01234567-89ab-cdef-0123-456789abcdef",
            ] {
                assert!(valid_uuid(value));
            }
            for value in [
                "",
                "0123456789abcdef0123456789abcdef",
                "01234567-89AB-cdef-0123-456789abcdef",
                "01234567_89ab-cdef-0123-456789abcdef",
                "01234567-89ab-cdef-0123-456789abcdeg",
                "01234567-89ab-cdef-0123-456789abcde\n",
                "01234567-89ab-cdef-0123-456789abcdé",
            ] {
                assert!(!valid_uuid(value), "{value:?}");
            }
        }
    }
}

/// Bounded JSON parsing, canonical encoding, and validated wire conversions.
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

    /// Temporary JSON ownership that erases strings on every exit path.
    struct JsonScratch(Value);

    impl Drop for JsonScratch {
        /// Recursively clear secret-bearing JSON before freeing its allocations.
        fn drop(&mut self) {
            /// Wipe strings nested in arrays or object values.
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

    /// Current nesting depth for duplicate-rejecting JSON deserialization.
    struct Strict(usize);

    impl<'de> DeserializeSeed<'de> for Strict {
        type Value = Value;

        /// Dispatch JSON values through this depth-bounded visitor.
        fn deserialize<D: serde::Deserializer<'de>>(
            self,
            d: D,
        ) -> std::result::Result<Value, D::Error> {
            d.deserialize_any(self)
        }
    }

    impl<'de> Visitor<'de> for Strict {
        type Value = Value;

        /// Describe the accepted shape without exposing rejected data.
        fn expecting(&self, f: &mut fmt::Formatter) -> fmt::Result {
            f.write_str("bounded JSON")
        }

        /// Preserve a JSON boolean.
        fn visit_bool<E: de::Error>(self, v: bool) -> std::result::Result<Value, E> {
            Ok(Value::Bool(v))
        }

        /// Preserve an unsigned JSON integer.
        fn visit_u64<E: de::Error>(self, v: u64) -> std::result::Result<Value, E> {
            Ok(v.into())
        }

        /// Preserve a signed JSON integer.
        fn visit_i64<E: de::Error>(self, v: i64) -> std::result::Result<Value, E> {
            Ok(v.into())
        }

        /// Reject nonfinite floating-point numbers.
        fn visit_f64<E: de::Error>(self, v: f64) -> std::result::Result<Value, E> {
            serde_json::Number::from_f64(v)
                .map(Value::Number)
                .ok_or_else(|| E::custom("number"))
        }

        /// Copy a borrowed JSON string into the parsed value.
        fn visit_str<E: de::Error>(self, v: &str) -> std::result::Result<Value, E> {
            Ok(Value::String(v.into()))
        }

        /// Take ownership of a decoded JSON string.
        fn visit_string<E: de::Error>(self, v: String) -> std::result::Result<Value, E> {
            Ok(Value::String(v))
        }

        /// Represent JSON null without allocating.
        fn visit_unit<E: de::Error>(self) -> std::result::Result<Value, E> {
            Ok(Value::Null)
        }

        /// Bound array nesting and clear partial values on rejection.
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

        /// Reject duplicate object keys and erase partially decoded values.
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

    /// Parse one bounded JSON value, rejecting duplicate keys and excessive nesting.
    /// Application-specific DTO decoders also use this shared parsing policy.
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

    /// Decode a typed record after enforcing the shared JSON syntax policy.
    fn decode<T: serde::de::DeserializeOwned>(b: &[u8], limit: usize) -> Result<T> {
        serde_json::from_value(strict_json(b, limit)?).map_err(|_| Error::InvalidRequest)
    }

    /// Encode bounded JSON using the same line-separator escaping as Go.
    fn encode<T: Serialize>(v: &T, limit: usize) -> Result<Vec<u8>> {
        /// Stop serialization before its output exceeds the caller's byte bound.
        struct Bounded {
            bytes: Vec<u8>,

            limit: usize,
        }

        impl std::io::Write for Bounded {
            /// Append only bytes that fit in the remaining output budget.
            fn write(&mut self, b: &[u8]) -> std::io::Result<usize> {
                if b.len() > self.limit - self.bytes.len() {
                    return Err(std::io::ErrorKind::FileTooLarge.into());
                }
                self.bytes.extend_from_slice(b);
                Ok(b.len())
            }

            /// Memory-backed output has no pending writes to flush.
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

    /// Require canonical UUID spelling without imposing version policy.
    fn uuid(s: &str) -> Result<()> {
        if valid_uuid(s) {
            Ok(())
        } else {
            Err(Error::InvalidRequest)
        }
    }

    /// Check the schema version before validating the cluster identity.
    fn header(v: u32, c: &str) -> Result<()> {
        if v != SCHEMA_VERSION {
            return Err(Error::IncompatibleMembership);
        }
        uuid(c)
    }

    /// Parse a nonzero decimal cursor with no alternative spellings.
    fn counter(s: &str) -> Result<u64> {
        let n = s.parse::<u64>().map_err(|_| Error::InvalidRequest)?;
        if n == 0 || n.to_string() != s {
            return Err(Error::InvalidRequest);
        }
        Ok(n)
    }

    /// Decode canonical padded base64 for nonsecret wire bytes.
    fn bytes(s: &str) -> Result<Vec<u8>> {
        let b = STANDARD.decode(s).map_err(|_| Error::InvalidRequest)?;
        if STANDARD.encode(&b) != s {
            return Err(Error::InvalidRequest);
        }
        Ok(b)
    }

    /// Decode exactly one canonical key into fixed-size zeroizing storage.
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

    /// Require a nonempty list of fully consumed DER certificates.
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

    /// Exact-name enrollment request representation used by serde.
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

    /// Decode enrollment syntax, NIC bounds, and a complete certificate request.
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

    /// Encode enrollment only after validating its canonical wire representation.
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

    /// Exact-name enrollment response with base64 certificate bytes.
    #[derive(Serialize, Deserialize)]
    #[serde(deny_unknown_fields)]
    struct Response {
        schema_version: u32,

        cluster: String,

        node: String,

        enrollment: String,

        certificate_chain: Vec<String>,

        #[serde(
            default,
            skip_serializing_if = "Option::is_none",
            deserialize_with = "nonnull_gid"
        )]
        block_devices: Option<String>,
    }

    /// Decode enrollment correlation and certificate syntax without establishing trust.
    pub fn decode_enrollment_response(b: &[u8]) -> Result<EnrollmentResponse> {
        let r: Response = decode(b, MAX_ENROLLMENT_BYTES)?;
        header(r.schema_version, &r.cluster)?;
        uuid(&r.node)?;
        uuid(&r.enrollment)?;
        let chain = certificates(&r.certificate_chain)?;
        Ok(EnrollmentResponse {
            block_devices: r.block_devices.filter(|s| !s.is_empty()),
            schema_version: r.schema_version,
            cluster: ClusterId(r.cluster),
            node: NodeId(r.node),
            enrollment: EnrollmentId(r.enrollment),
            certificate_chain: chain,
        })
    }

    /// Encode an enrollment response and enforce the same validation as decoding.
    pub fn encode_enrollment_response(r: &EnrollmentResponse) -> Result<Vec<u8>> {
        let b = encode(
            &Response {
                block_devices: r.block_devices.clone().filter(|s| !s.is_empty()),
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

    /// Physical NIC wire record with absent-but-not-null optional fields.
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

    /// Accept a present NUMA node only when it is an unsigned integer.
    fn nonnull_optional<'de, D: serde::Deserializer<'de>>(
        d: D,
    ) -> std::result::Result<Option<u32>, D::Error> {
        u32::deserialize(d).map(Some)
    }

    /// Accept a present GID only when it is a string, never explicit null.
    fn nonnull_gid<'de, D: serde::Deserializer<'de>>(
        d: D,
    ) -> std::result::Result<Option<String>, D::Error> {
        String::deserialize(d).map(Some)
    }

    /// Validate physical NIC identities and sort their logical rail mappings.
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
                    wire_codec::decode_hex(value.as_bytes()).map_err(|_| Error::InvalidRequest)
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

    /// Encode optional NIC fields and sort without mutating caller-owned mappings.
    fn nics_to_dto(nics: &[RailMapping]) -> Result<Vec<Rail>> {
        let mut result = nics
            .iter()
            .map(|nic| {
                Ok(Rail {
                    device: nic.device.clone(),
                    port: nic.port,
                    rail: nic.rail.0,
                    gid: nic.gid.map(|gid| wire_codec::encode_hex(&gid)),
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

    /// Membership JSON record before syntax validation and normalization.
    #[derive(Clone, Serialize, Deserialize)]
    #[serde(deny_unknown_fields)]
    struct MemberDto {
        node: String,

        shares: u32,

        peer_endpoint: String,

        rdma_nics: Vec<Rail>,

        site: String,
    }

    /// Cache JSON record before canonical socket validation.
    #[derive(Clone, Serialize, Deserialize)]
    #[serde(deny_unknown_fields)]
    struct Cache {
        id: String,

        name: String,

        client_socket: String,

        origin_socket: String,
    }

    /// Full publication with decimal-string cursors for cross-language precision.
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

    /// Decode and normalize a bounded full publication.
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

    /// Build sorted JSON records without changing the caller's publication.
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

    /// Encode a full publication only when its records and byte size are valid.
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

    /// Produce stable content and membership bytes independent of cursor values.
    pub fn canonical_content(p: &Publication) -> Result<(Vec<u8>, Vec<u8>)> {
        let mut d = dto(p)?;
        d.sequence = "1".into();
        d.membership_version = "1".into();
        encode(&d, MAX_PUBLICATION_BYTES)?;
        publication_from_dto(d.clone())?;
        /// Canonical publication content without changing cursors.
        #[derive(Serialize)]
        struct Content<'a> {
            schema_version: u32,

            cluster: &'a str,

            members: &'a [MemberDto],

            caches: &'a [Cache],
        }

        /// Canonical membership bytes exclude both cursors and cache definitions.
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

    /// Incremental publication bound to exact base and target content hashes.
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

    /// Hash the canonical publication content using lowercase SHA-256 hexadecimal.
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

    /// Complete wire keyring with encoded certificates and zeroizing key fields.
    #[derive(Serialize, Deserialize)]
    #[serde(deny_unknown_fields)]
    struct Bundle {
        schema_version: u32,

        cluster: String,

        generation: String,

        peer_trust_roots: Vec<String>,

        cache_keys: Vec<Key>,
    }

    /// One key record whose material is owned even during partial deserialization.
    #[derive(Serialize, Deserialize)]
    #[serde(deny_unknown_fields)]
    struct Key {
        cache: String,

        id: String,

        purpose: String,

        state: String,

        material: EncodedKeyMaterial,
    }

    /// Own encoded secrets immediately, including during partial DTO construction.
    struct EncodedKeyMaterial(zeroize::Zeroizing<String>);

    impl Serialize for EncodedKeyMaterial {
        /// Preserve the existing base64 string representation.
        fn serialize<S: serde::Serializer>(
            &self,
            serializer: S,
        ) -> std::result::Result<S::Ok, S::Error> {
            serializer.serialize_str(&self.0)
        }
    }

    impl<'de> Deserialize<'de> for EncodedKeyMaterial {
        /// Immediately wrap decoded string ownership so later field errors wipe it.
        fn deserialize<D: serde::Deserializer<'de>>(
            deserializer: D,
        ) -> std::result::Result<Self, D::Error> {
            String::deserialize(deserializer).map(|value| Self(zeroize::Zeroizing::new(value)))
        }
    }

    /// Decode a bounded bundle with canonical keys and exactly one active key per group.
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
            let created = KeyId(id).generation().ok_or(Error::InvalidRequest)?;
            if created > generation.0 {
                return Err(Error::InvalidRequest);
            }
            let material = key_material(&k.material.0)?;
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

    /// Encode and validate a keyring; returned bytes contain caller-owned secrets.
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
                    material: EncodedKeyMaterial(zeroize::Zeroizing::new(
                        STANDARD.encode(k.material),
                    )),
                })
                .collect(),
        };
        /// Bounded secret output that is erased unless successfully transferred.
        struct BundleOutput(zeroize::Zeroizing<Vec<u8>>);

        impl std::io::Write for BundleOutput {
            /// Refuse bytes beyond the fixed bundle capacity before copying secrets.
            fn write(&mut self, bytes: &[u8]) -> std::io::Result<usize> {
                if bytes.len() > MAX_BUNDLE_BYTES - self.0.len() {
                    return Err(std::io::ErrorKind::FileTooLarge.into());
                }
                self.0.extend_from_slice(bytes);
                Ok(bytes.len())
            }

            /// In-memory serialization has no pending external writes.
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

    /// Decode a known control error code without retaining the input payload.
    pub fn decode_error(b: &[u8]) -> Result<ErrorResponse> {
        /// Exact-name error DTO rejecting any extra response fields.
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

    /// Encode the stable snake-case spelling of a control failure.
    pub fn encode_error(response: &ErrorResponse) -> Result<Vec<u8>> {
        /// Borrow the static wire spelling without allocating another string.
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
    std::thread_local! {
        /// Count field destruction without observing freed secret storage.
        static MATERIAL_DROPS: std::cell::Cell<usize> = const { std::cell::Cell::new(0) };
    }

    #[cfg(test)]
    impl Drop for EncodedKeyMaterial {
        /// Observe field destruction without examining freed secret bytes.
        fn drop(&mut self) {
            MATERIAL_DROPS.with(|drops| drops.set(drops.get() + 1));
            // The Zeroizing field is dropped immediately after this test observer.
        }
    }

    /// Delta state transitions and replay protection against canonical content.
    #[cfg(test)]
    mod delta_tests {
        use super::*;

        /// Site-only updates preserve exact content hashes and reject altered labels.
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

        /// Mixed member edits bind the base, target hash, and advancing cursor.
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

    /// Cross-language fixture contracts and boundary-sized wire records.
    #[cfg(test)]
    mod contract_tests {
        use super::*;
        use crate::canonical_socket_paths;
        use sha2::{Digest, Sha256};

        /// Location of Go-owned shared contract fixtures, never regenerated by Rust.
        const ROOT: &str = concat!(
            env!("CARGO_MANIFEST_DIR"),
            "/../../../internal/racer/wire/testdata/"
        );

        /// Canonical publication content and membership match Go's exact bytes.
        #[test]
        fn shared_publication_vectors() {
            let p = decode_publication(fixture("publication.json").as_slice()).unwrap();
            let (content, membership) = canonical_content(&p).unwrap();
            assert_eq!(content, fixture("content.json"));
            assert_eq!(membership, fixture("membership.json"));
            let hashes: Value = serde_json::from_slice(&fixture("hashes.json")).unwrap();
            let (ph, mh) = content_hashes(&p).unwrap();
            assert_eq!(ph, hashes["content"]);
            assert_eq!(mh, hashes["membership"]);
            let b = encode_publication(&p).unwrap();
            assert_eq!(b, round_trip("publication.json", &b).unwrap());
        }

        /// Site additions, changes, and removals agree with Go and reject replay.
        #[test]
        fn shared_site_vectors() {
            // Generated by Go's opt-in TestGenerateSharedSiteVectors, never by Rust.
            // JSON strings preserve canonical field ordering for byte-for-byte comparisons.
            /// One Go-produced site transition with canonical bytes and hashes.
            #[derive(Deserialize)]
            struct SiteVector {
                name: String,

                site: String,

                publication: String,

                content: String,

                membership: String,

                content_hash: String,

                membership_hash: String,

                delta: Option<String>,
            }
            let vectors: Vec<SiteVector> =
                serde_json::from_slice(&fixture("site-vectors.json")).unwrap();
            assert_eq!(vectors.len(), 4);
            let legacy = decode_publication(&fixture("publication.json")).unwrap();
            let mut previous = None;
            for (i, (name, site)) in [
                ("absent", ""),
                ("added", "Site_1.west-2"),
                ("changed", "Site_2.east-1"),
                ("removed", ""),
            ]
            .into_iter()
            .enumerate()
            {
                let v = &vectors[i];
                assert_eq!(v.name, name);
                assert_eq!(v.site, site);
                let bytes = v.publication.as_bytes();
                let p = decode_publication(bytes).unwrap();
                let mut want = legacy.clone();
                want.sequence.0 = (i + 1) as u64;
                want.membership_version.0 = (i + 1) as u64;
                want.members[0].site = site.into();
                assert_eq!(encode_publication(&want).unwrap(), bytes, "{name}");
                assert_eq!(encode_publication(&p).unwrap(), bytes, "{name}");
                let (content, membership) = canonical_content(&p).unwrap();
                assert_eq!(content, v.content.as_bytes(), "{name}");
                assert_eq!(membership, v.membership.as_bytes(), "{name}");
                let (ph, mh) = content_hashes(&p).unwrap();
                assert_eq!(ph, v.content_hash, "{name}");
                assert_eq!(mh, v.membership_hash, "{name}");
                if let Some(base) = &previous {
                    assert_ne!(ph, vectors[i - 1].content_hash);
                    assert_ne!(mh, vectors[i - 1].membership_hash);
                    let delta = v.delta.as_ref().unwrap().as_bytes();
                    let applied = apply_delta(base, delta).unwrap();
                    assert_eq!(encode_publication(&applied).unwrap(), bytes, "{name}");
                    let mut d: Value = serde_json::from_slice(delta).unwrap();
                    assert_eq!(d["upsert_members"].as_array().unwrap().len(), 1);
                    assert!(d["remove_members"].as_array().unwrap().is_empty());
                    assert_eq!(d["upsert_members"][0]["site"], site);
                    d["upsert_members"][0]["site"] = "tampered-site".into();
                    assert_eq!(
                        apply_delta(base, &serde_json::to_vec(&d).unwrap()).err(),
                        Some(Error::Replay),
                        "Site is bound to the target hash: {name}"
                    );
                    let mut wrong_base = base.clone();
                    wrong_base.members[0].site = "stale-site".into();
                    assert_eq!(apply_delta(&wrong_base, delta).err(), Some(Error::Replay));
                    assert_eq!(apply_delta(&p, delta).err(), Some(Error::Replay));
                } else {
                    assert!(v.delta.is_none());
                }
                previous = Some(p);
            }
            assert_eq!(vectors[0].content, vectors[3].content);
            assert_eq!(vectors[0].membership, vectors[3].membership);
            assert_eq!(vectors[0].content_hash, vectors[3].content_hash);
            assert_eq!(vectors[0].membership_hash, vectors[3].membership_hash);
        }

        /// Site spelling, empty values, and type rejection remain hash-visible.
        #[test]
        fn site_wire_defaults_validation_and_hashes() {
            let mut p = decode_publication(&fixture("publication.json")).unwrap();
            assert!(p.members.iter().all(|m| m.site.is_empty()));
            let legacy = encode_publication(&p).unwrap();
            assert!(
                String::from_utf8(legacy.clone())
                    .unwrap()
                    .contains("\"site\"")
            );
            let original = content_hashes(&p).unwrap();
            for site in ["west", "Site_1.west-2", &"A".repeat(63)] {
                p.members[0].site = site.into();
                let encoded = encode_publication(&p).unwrap();
                assert_eq!(decode_publication(&encoded).unwrap().members[0].site, site);
                let changed = content_hashes(&p).unwrap();
                assert_ne!(changed.0, original.0);
                assert_ne!(changed.1, original.1);
            }
            for site in ["-a", "a-", "a_", ".a", "a/b", "é", &"a".repeat(64)] {
                p.members[0].site = site.into();
                assert_eq!(encode_publication(&p), Err(Error::InvalidRequest));
                let mut json: Value = serde_json::from_slice(&legacy).unwrap();
                json["members"][0]["site"] = site.into();
                assert!(decode_publication(&serde_json::to_vec(&json).unwrap()).is_err());
            }
            p.members[0].site.clear();
            assert_eq!(encode_publication(&p).unwrap(), legacy);
            for value in ["null", "1", "true", "[]", "{}", "\"a\",\"site\":\"b\""] {
                let raw = String::from_utf8(legacy.clone()).unwrap().replacen(
                    "\"site\":\"\"",
                    &format!("\"site\":{value}"),
                    1,
                );
                assert!(decode_publication(raw.as_bytes()).is_err(), "{value}");
            }
        }

        /// Enrollment and key bundles retain exact canonical fixture bytes.
        #[test]
        fn shared_bootstrap_and_bundle_vectors() {
            for name in [
                "bootstrap-request.json",
                "bootstrap-response.json",
                "bundle.json",
            ] {
                let b = fixture(name);
                assert_eq!(round_trip(name, &b).unwrap(), b, "{name}");
            }
        }

        #[test]
        fn optional_block_device_selector_preserves_legacy_and_rejects_non_strings() {
            let legacy = fixture("bootstrap-response.json");
            let mut response = decode_enrollment_response(&legacy).unwrap();
            assert!(response.block_devices.is_none());
            for selector in ["nvme.*cache", "[", ""] {
                response.block_devices = Some(selector.into());
                let encoded = encode_enrollment_response(&response).unwrap();
                let decoded = decode_enrollment_response(&encoded).unwrap();
                assert_eq!(
                    decoded.block_devices.as_deref(),
                    (!selector.is_empty()).then_some(selector)
                );
                if selector.is_empty() {
                    assert_eq!(encoded, legacy);
                }
            }
            for value in [
                Value::Null,
                serde_json::json!(1),
                serde_json::json!(true),
                serde_json::json!([]),
            ] {
                let mut json: Value = serde_json::from_slice(&legacy).unwrap();
                json["block_devices"] = value;
                assert!(decode_enrollment_response(&serde_json::to_vec(&json).unwrap()).is_err());
            }
        }

        #[test]
        fn shared_block_device_selector_vectors() {
            let legacy = String::from_utf8(fixture("bootstrap-response.json")).unwrap();
            let vectors: Vec<Value> =
                serde_json::from_slice(&fixture("bootstrap-block-devices.json")).unwrap();
            for vector in vectors {
                let bytes = format!(
                    "{}{} }}",
                    legacy.trim_end().strip_suffix('}').unwrap(),
                    vector["fields"].as_str().unwrap()
                );
                let decoded = decode_enrollment_response(bytes.as_bytes());
                if vector["code"] == "" {
                    let decoded = decoded.unwrap();
                    assert_eq!(
                        decoded.block_devices.as_deref().unwrap_or(""),
                        vector["pattern"].as_str().unwrap()
                    );
                    let encoded = encode_enrollment_response(&decoded).unwrap();
                    assert_eq!(
                        decode_enrollment_response(&encoded).unwrap().block_devices,
                        decoded.block_devices
                    );
                } else {
                    assert!(
                        matches!(decoded, Err(Error::InvalidRequest)),
                        "{}",
                        vector["name"]
                    );
                }
            }
        }

        /// NIC limits count physical ports, permit shared rails, and normalize order.
        #[test]
        fn physical_nic_bounds_repeated_rails_and_bootstrap_canonical_order() {
            let mut p = decode_publication(&fixture("publication.json")).unwrap();
            let nic = |device: String| RailMapping {
                device,
                port: 255,
                rail: RailId(65535),
                gid: Some([0xab; 16]),
                numa_node: Some(u32::MAX as usize),
            };
            p.members[0].rails = (0..64).rev().map(|i| nic(format!("mlx5_{i:02}"))).collect();
            let encoded = encode_publication(&p).unwrap();
            let decoded = decode_publication(&encoded).unwrap();
            assert_eq!(decoded.members[0].rails.len(), 64);
            assert_eq!(decoded.members[0].rails[0].device, "mlx5_00");
            assert_eq!(p.members[0].rails[0].device, "mlx5_63");
            let mut request =
                decode_enrollment_request(&fixture("bootstrap-request.json")).unwrap();
            request.rdma_nics = p.members[0].rails.clone();
            let encoded = encode_enrollment_request(&request).unwrap();
            assert_eq!(
                decode_enrollment_request(&encoded).unwrap().rdma_nics[0].device,
                "mlx5_00"
            );
            for (field, value) in [
                ("gid", serde_json::json!("")),
                ("gid", Value::Null),
                ("gid", serde_json::json!("AB".repeat(16))),
                ("numa_node", Value::Null),
                ("port", serde_json::json!(0)),
                ("port", serde_json::json!(256)),
            ] {
                let mut value_json: Value = serde_json::from_slice(&encoded).unwrap();
                value_json["rdma_nics"][0][field] = value;
                assert!(
                    decode_enrollment_request(&serde_json::to_vec(&value_json).unwrap()).is_err()
                );
            }
            p.members[0].rails.push(nic("extra".into()));
            assert_eq!(encode_publication(&p), Err(Error::Overloaded));
            request.rdma_nics.push(nic("extra".into()));
            assert_eq!(encode_enrollment_request(&request), Err(Error::Overloaded));
        }

        /// Shared malformed records retain the exact cross-language error categories.
        #[test]
        fn shared_rejection_vectors() {
            let cases: Value = serde_json::from_slice(&fixture("rejections.json")).unwrap();
            for case in cases.as_array().unwrap() {
                let name = case["name"].as_str().unwrap();
                let file = case["file"].as_str().unwrap();
                let input = String::from_utf8(fixture(file)).unwrap();
                let mutated = input.replacen(
                    case["old"].as_str().unwrap(),
                    case["new"].as_str().unwrap(),
                    1,
                );
                assert_ne!(input, mutated, "mutation did not match: {name}");
                // The client maps wire version and size failures to its local error domain.
                let expected = match case["code"].as_str().unwrap() {
                    "unsupported_version" => Error::IncompatibleMembership,
                    "too_large" => Error::Overloaded,
                    "invalid_request" => Error::InvalidRequest,
                    code => panic!("unexpected rejection code: {code}"),
                };
                assert_eq!(
                    round_trip(file, mutated.as_bytes()).err(),
                    Some(expected),
                    "{name}"
                );
            }
        }

        /// Retirement is expressed by omission, never by an unsupported key state.
        #[test]
        fn retiring_state_is_rejected_with_active_key_present() {
            let input = String::from_utf8(fixture("bundle.json")).unwrap();
            let changed = input.replacen("\"state\":\"prepared\"", "\"state\":\"retiring\"", 1);
            assert_ne!(changed, input);
            assert_eq!(
                decode_bundle(changed.as_bytes()).err(),
                Some(Error::InvalidRequest)
            );
        }

        /// Hashes ignore ordering and cursors but bind cache and endpoint changes.
        #[test]
        fn hash_semantics() {
            let mut p = decode_publication(fixture("publication.json").as_slice()).unwrap();
            let hashes = content_hashes(&p).unwrap();
            p.sequence.0 = 0;
            p.membership_version.0 = 0;
            p.members.reverse();
            p.members[0].rails.reverse();
            assert_eq!(content_hashes(&p).unwrap(), hashes);
            assert_eq!(p.members[0].rails[0].rail.0, 65535, "caller input mutated");
            p.caches[0].id = CacheId("66666666-6666-4666-8666-666666666666".into());
            let changed = content_hashes(&p).unwrap();
            assert_ne!(changed.0, hashes.0);
            assert_eq!(changed.1, hashes.1);
            p.members[0].peer_endpoint = "[2001:db8::2]:7443".into();
            let endpoint = content_hashes(&p).unwrap();
            assert_ne!(endpoint.0, changed.0);
            assert_ne!(endpoint.1, changed.1);
        }

        /// Locally constructed publications follow the same validation as wire input.
        #[test]
        fn local_publications_share_wire_validation_and_normalization() {
            let original = decode_publication(&fixture("publication.json")).unwrap();
            let mut unsorted = original.clone();
            unsorted.members.reverse();
            for member in &mut unsorted.members {
                member.rails.reverse();
            }
            let normalized = validate_publication(&unsorted).unwrap();
            assert_eq!(
                encode_publication(&normalized).unwrap(),
                encode_publication(&original).unwrap()
            );
            for mutation in 0..8 {
                let mut candidate = original.clone();
                match mutation {
                    0 => candidate.schema_version += 1,
                    1 => candidate.cluster.0 = "invalid".into(),
                    2 => candidate.sequence.0 = 0,
                    3 => candidate.membership_version.0 = 0,
                    4 => candidate.members.push(candidate.members[0].clone()),
                    5 => candidate.members[0].peer_endpoint = "host:443".into(),
                    6 => {
                        let member = candidate
                            .members
                            .iter_mut()
                            .find(|m| !m.rails.is_empty())
                            .unwrap();
                        let duplicate = member.rails[0].clone();
                        member.rails.push(duplicate);
                    }
                    _ => candidate.caches[0].client_socket = "/unexpected/socket".into(),
                }
                let bytes = encode(&dto(&candidate).unwrap(), MAX_PUBLICATION_BYTES).unwrap();
                let expected = decode_publication(&bytes).err();
                assert!(expected.is_some(), "mutation {mutation}");
                assert_eq!(
                    validate_publication(&candidate).err(),
                    expected,
                    "mutation {mutation}"
                );
                assert_eq!(
                    encode_publication(&candidate).err(),
                    expected,
                    "mutation {mutation}"
                );
            }
        }

        /// Exact byte limits succeed while malformed or oversized documents fail.
        #[test]
        fn byte_bounds_and_malformed_documents() {
            for (name, max) in [
                ("publication.json", MAX_PUBLICATION_BYTES),
                ("bootstrap-request.json", MAX_ENROLLMENT_BYTES),
                ("bootstrap-response.json", MAX_ENROLLMENT_BYTES),
                ("bundle.json", MAX_BUNDLE_BYTES),
            ] {
                let b = fixture(name);
                let mut padded = b.clone();
                padded.resize(max, b' ');
                assert!(round_trip(name, &padded).is_ok(), "exact bound {name}");
                padded.push(b' ');
                assert_eq!(round_trip(name, &padded).err(), Some(Error::Overloaded));
                for bad in [
                    Vec::new(),
                    b"null".to_vec(),
                    b"[]".to_vec(),
                    b[..b.len() - 1].to_vec(),
                    [b.as_slice(), b"{}"].concat(),
                    [b.as_slice(), &[0xff]].concat(),
                    format!("{}{}", "[".repeat(1000), "]".repeat(1000)).into_bytes(),
                ] {
                    assert_eq!(
                        round_trip(name, &bad).err(),
                        Some(Error::InvalidRequest),
                        "{name}"
                    );
                }
            }
            // Runtime decoders accept already bounded byte slices, not Read adapters.
            let source = vec![0; MAX_ENROLLMENT_BYTES * 10];
            assert_eq!(
                decode_enrollment_request(&source).err(),
                Some(Error::Overloaded)
            );
        }

        /// Socket names, membership counts, and error codes retain strict bounds.
        #[test]
        fn path_member_and_enum_bounds() {
            for name in [
                ".",
                "..",
                "a/b",
                "a\\b",
                "a\0b",
                "A",
                "-a",
                "a-",
                "a..b",
                &"a".repeat(64),
            ] {
                assert!(canonical_socket_paths(name).is_err());
            }
            let name = format!("{}.{}", "a".repeat(63), "b".repeat(18));
            assert_eq!(
                canonical_socket_paths(&name).unwrap().0.as_os_str().len(),
                107
            );
            assert!(canonical_socket_paths(&(name + "b")).is_err());
            let mut p = decode_publication(fixture("publication.json").as_slice()).unwrap();
            p.members.resize(MAX_MEMBERS + 1, p.members[0].clone());
            assert_eq!(encode_publication(&p).err(), Some(Error::Overloaded));
            for code in [
                "invalid_request",
                "unauthenticated",
                "forbidden",
                "conflict",
                "too_large",
                "unsupported_version",
                "overloaded",
                "unavailable",
            ] {
                let b = format!(r#"{{"code":"{code}"}}"#);
                assert_eq!(
                    encode_error(&decode_error(b.as_bytes()).unwrap()).unwrap(),
                    b.as_bytes()
                );
            }
            assert!(decode_error(br#"{"code":"future"}"#.as_slice()).is_err());
        }

        /// The maximum accepted membership round-trips; one additional member fails.
        #[test]
        fn maximum_membership() {
            let mut p = decode_publication(fixture("publication.json").as_slice()).unwrap();
            p.members = (0..MAX_MEMBERS)
                .map(|i| Member {
                    node: NodeId(format!("{i:08x}-1111-4111-8111-111111111111")),
                    shares: NonZeroU32::new(1).unwrap(),
                    peer_endpoint: "192.0.2.1:1".into(),
                    rails: vec![],
                    site: String::new(),
                })
                .collect();
            let b = encode_publication(&p).unwrap();
            assert_eq!(
                decode_publication(b.as_slice()).unwrap().members.len(),
                MAX_MEMBERS
            );
            let mut dto = dto(&p).unwrap();
            let extra: MemberDto =
                serde_json::from_value(serde_json::to_value(&dto.members[0]).unwrap()).unwrap();
            dto.members.push(extra);
            let b = encode(&dto, MAX_PUBLICATION_BYTES).unwrap();
            assert_eq!(
                decode_publication(b.as_slice()).err(),
                Some(Error::Overloaded)
            );
        }

        /// Hash the two canonical byte views independently for fixture comparisons.
        fn content_hashes(p: &Publication) -> Result<(String, String)> {
            let (content, membership) = canonical_content(p)?;
            Ok((
                format!("{:x}", Sha256::digest(content)),
                format!("{:x}", Sha256::digest(membership)),
            ))
        }

        /// Read one shared fixture, removing only its optional final newline.
        fn fixture(name: &str) -> Vec<u8> {
            let mut b = std::fs::read(format!("{ROOT}{name}")).unwrap();
            if b.last() == Some(&b'\n') {
                b.pop();
            }
            b
        }

        /// Decode and re-encode the record family selected by the fixture name.
        fn round_trip(name: &str, b: &[u8]) -> Result<Vec<u8>> {
            match name {
                "publication.json" => encode_publication(&decode_publication(b)?),
                "bootstrap-request.json" => {
                    encode_enrollment_request(&decode_enrollment_request(b)?)
                }
                "bootstrap-response.json" => {
                    encode_enrollment_response(&decode_enrollment_response(b)?)
                }
                "bundle.json" => encode_bundle(&decode_bundle(b)?),
                _ => panic!("unknown vector"),
            }
        }
    }

    /// Codec boundaries and shared vectors, including secret cleanup on failure.
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

        /// NIC GIDs round-trip only with exact lowercase hexadecimal spelling.
        #[test]
        fn gid_hex_round_trip_preserves_bytes_and_rejects_noncanonical_input() {
            let gid = std::array::from_fn(|i| (i as u8) * 17);
            let nic = RailMapping {
                device: "mlx5_0".into(),
                port: 1,
                rail: RailId(0),
                gid: Some(gid),
                numa_node: None,
            };
            let dto = nics_to_dto(std::slice::from_ref(&nic)).unwrap();
            assert_eq!(
                dto[0].gid.as_deref(),
                Some("00112233445566778899aabbccddeeff")
            );
            assert_eq!(nics_from_dto(dto).unwrap(), vec![nic.clone()]);
            let mut absent = nic.clone();
            absent.gid = None;
            assert_eq!(
                nics_from_dto(nics_to_dto(std::slice::from_ref(&absent)).unwrap()).unwrap(),
                vec![absent]
            );
            for value in [
                "".to_owned(),
                "0".repeat(31),
                "0".repeat(33),
                "AB".repeat(16),
                format!("{}g0", "00".repeat(15)),
                format!("{} 0", "00".repeat(15)),
                format!("{}é", "00".repeat(15)),
            ] {
                let mut dto = nics_to_dto(std::slice::from_ref(&nic)).unwrap();
                dto[0].gid = Some(value);
                assert_eq!(nics_from_dto(dto), Err(Error::InvalidRequest));
            }
        }

        /// Key syntax and future epochs preserve invalid-request classification.
        #[test]
        fn bundle_key_generation_maps_syntax_and_future_epochs_to_invalid_request() {
            let mut bundle: Value = serde_json::from_str(BUNDLE).unwrap();
            bundle["generation"] = "1".into();
            bundle["cache_keys"].as_array_mut().unwrap().truncate(1);
            bundle["cache_keys"][0]["state"] = "active".into();
            bundle["cache_keys"][0]["id"] = STANDARD
                .encode(KeyId::from_generation(1, 0).unwrap().0)
                .into();
            assert!(decode_bundle(&serde_json::to_vec(&bundle).unwrap()).is_ok());
            for id in [
                KeyId([1; 16]),
                KeyId(*b"RKG1\0\0\0\0\0\0\0\0\0\0\0\0"),
                KeyId::from_generation(2, 0).unwrap(),
            ] {
                bundle["cache_keys"][0]["id"] = STANDARD.encode(id.0).into();
                assert!(matches!(
                    decode_bundle(&serde_json::to_vec(&bundle).unwrap()),
                    Err(Error::InvalidRequest)
                ));
            }
        }

        /// Secret base64 must have the exact length, padding, and unused-bit values.
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

        /// Missing or invalid later fields must drop the already-owned secret field.
        #[test]
        fn encoded_material_is_owned_before_key_construction_completes() {
            for suffix in [
                "",
                r#", "state": false"#,
                r#", "state": "active", "unknown": 1"#,
            ] {
                let json = format!(
                    r#"{{"cache":"cache","id":"id","purpose":"page","material":"secret"{suffix}}}"#
                );
                MATERIAL_DROPS.with(|drops| drops.set(0));
                // Deserialize from Value exactly as decode_bundle does, preserving
                // material-before-state ordering even with sorted JSON object keys.
                let value: Value = serde_json::from_str(&json).unwrap();
                assert!(Key::deserialize(&value).is_err());
                assert_eq!(MATERIAL_DROPS.with(|drops| drops.get()), 1);
            }
            MATERIAL_DROPS.with(|drops| drops.set(0));
            let key: Key = serde_json::from_str(r#"{"cache":"cache","id":"id","purpose":"page","material":"secret","state":"active"}"#).unwrap();
            assert_eq!(key.material.0.as_str(), "secret");
            assert_eq!(MATERIAL_DROPS.with(|drops| drops.get()), 0);
            drop(key);
            assert_eq!(MATERIAL_DROPS.with(|drops| drops.get()), 1);
        }

        /// Public bundle decoding preserves error classification after secret parsing.
        #[test]
        fn bundle_rejects_missing_state_and_late_errors_after_material() {
            for replacement in ["", r#""state":false,"#] {
                let malformed = BUNDLE.replacen(r#""state":"prepared","#, replacement, 1);
                assert_ne!(malformed, BUNDLE);
                MATERIAL_DROPS.with(|drops| drops.set(0));
                assert!(matches!(
                    decode_bundle(malformed.as_bytes()),
                    Err(Error::InvalidRequest)
                ));
                assert!(MATERIAL_DROPS.with(|drops| drops.get()) > 0);
            }
        }

        /// The Go delta fixture applies exactly once and binds changed member content.
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

        /// Publications, enrollment, and bundles preserve Go's canonical bytes.
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

        /// Reject syntax aliases, duplicates, extra fields, and byte-limit violations.
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

        /// Bundles reject ambiguous active keys and unsupported states or purposes.
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

/// Ownership transfer preserves key identity without exposing borrowed secret bytes.
#[cfg(test)]
mod transfer_tests {
    use super::*;

    /// Consuming transfer returns zeroizing material and unchanged key metadata.
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
