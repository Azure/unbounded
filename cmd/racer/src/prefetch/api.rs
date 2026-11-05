//! Prefetch JSON: strict PUT body parsing and status serialization.

use super::{Config, Priority};
use crate::model::{CacheKey, StrongEtag};
use racer_wire::object::prefetch::{ErrorCode, PrefetchError};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};

/// Largest `generation` and `sequence` (JSON-safe integers).
pub const MAX_COUNTER: u64 = (1 << 53) - 1;

/// Default `ttl_seconds`.
pub const DEFAULT_TTL_SECS: u64 = 600;

#[derive(Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
struct RawSpec {
    #[serde(default, skip_serializing_if = "Option::is_none")]
    cache_uid: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    priority: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    generation: Option<u64>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    sequence: Option<u64>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    ttl_seconds: Option<u64>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    max_pending_bytes: Option<u64>,
    refs: Vec<RawRef>,
}

#[derive(Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
struct RawRef {
    key: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    etag: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    offset: Option<u64>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    length: Option<u64>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    metadata: Option<String>,
}

/// One validated ref.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct Ref {
    pub key: CacheKey,
    pub etag: Option<StrongEtag>,
    pub offset: u64,
    /// `None` means to the end of the object.
    pub length: Option<u64>,
    /// Opaque origin metadata, validated like `Racer-Metadata`.
    pub metadata: Option<String>,
}

/// A validated PUT body with defaults applied.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct Spec {
    pub cache_uid: Option<String>,
    pub priority: Priority,
    pub generation: u64,
    pub sequence: u64,
    /// Applied TTL, already capped at `RACER_PREFETCH_MAX_TTL_SECS`.
    pub ttl_seconds: u64,
    /// Applied pending bound, already defaulted and capped.
    pub max_pending_bytes: u64,
    pub refs: Vec<Ref>,
    /// SHA-256 of the canonical body plus the Authorization presence bit.
    pub hash: [u8; 32],
}

/// Parse and validate a PUT body. `authorization` is only its presence.
pub fn parse_spec(
    body: &[u8],
    authorization: bool,
    config: &Config,
) -> Result<Spec, PrefetchError> {
    let raw: RawSpec = serde_json::from_slice(body)
        .map_err(|error| PrefetchError::invalid(format!("invalid JSON body: {error}")))?;
    if raw.refs.is_empty() {
        return Err(PrefetchError::invalid("refs must not be empty"));
    }
    if raw.refs.len() as u64 > config.max_refs {
        return Err(PrefetchError::new(
            ErrorCode::TooLarge,
            format!("{} refs exceed {}", raw.refs.len(), config.max_refs),
        ));
    }
    let priority = match raw.priority.as_deref() {
        None => Priority::Normal,
        Some(value) => Priority::parse(value)
            .ok_or_else(|| PrefetchError::invalid("priority must be high, normal, or low"))?,
    };
    let counter = |value: Option<u64>, name: &str| match value.unwrap_or(0) {
        value if value <= MAX_COUNTER => Ok(value),
        _ => Err(PrefetchError::invalid(format!("{name} exceeds 2^53-1"))),
    };
    let generation = counter(raw.generation, "generation")?;
    let sequence = counter(raw.sequence, "sequence")?;
    let ttl_seconds = match raw.ttl_seconds.unwrap_or(DEFAULT_TTL_SECS) {
        0 => return Err(PrefetchError::invalid("ttl_seconds must be at least 1")),
        ttl => ttl.min(config.max_ttl_secs),
    };
    let max_pending_bytes = match raw.max_pending_bytes.unwrap_or(0) {
        0 => config.default_pending_bytes,
        bytes => bytes.min(config.max_pending_bytes),
    };
    if let Some(uid) = &raw.cache_uid
        && (uid.is_empty() || uid.len() > 64)
    {
        return Err(PrefetchError::invalid("invalid cache_uid"));
    }
    let mut refs = Vec::with_capacity(raw.refs.len());
    for (index, raw) in raw.refs.iter().enumerate() {
        let key = CacheKey::parse_hex(raw.key.as_bytes())
            .map_err(|_| PrefetchError::invalid(format!("refs[{index}].key is not 64 hex")))?;
        let etag = raw
            .etag
            .as_ref()
            .map(|etag| StrongEtag::parse(etag.as_bytes()))
            .transpose()
            .map_err(|_| PrefetchError::invalid(format!("refs[{index}].etag is not strong")))?;
        if raw.length == Some(0) {
            return Err(PrefetchError::invalid(format!(
                "refs[{index}].length must be at least 1"
            )));
        }
        if let Some(metadata) = &raw.metadata {
            crate::security::OpaqueMetadata::from_header(metadata.as_bytes()).map_err(|_| {
                PrefetchError::invalid(format!("refs[{index}].metadata is invalid"))
            })?;
        }
        refs.push(Ref {
            key,
            etag,
            offset: raw.offset.unwrap_or(0),
            length: raw.length,
            metadata: raw.metadata.clone(),
        });
    }
    // The canonical form is the re-serialized body: field order fixed, absent
    // fields omitted, whitespace removed.
    let canonical = serde_json::to_vec(&raw)
        .map_err(|_| PrefetchError::invalid("body cannot be canonicalized"))?;
    let mut hasher = Sha256::new();
    hasher.update(&canonical);
    hasher.update([u8::from(authorization)]);
    Ok(Spec {
        cache_uid: raw.cache_uid,
        priority,
        generation,
        sequence,
        ttl_seconds,
        max_pending_bytes,
        refs,
        hash: hasher.finalize().into(),
    })
}

/// Ticket state on the wire.
#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize)]
pub enum State {
    #[serde(rename = "QUEUED")]
    Queued,
    #[serde(rename = "RUNNING")]
    Running,
    #[serde(rename = "SUCCEEDED")]
    Succeeded,
    #[serde(rename = "PARTIAL")]
    Partial,
    #[serde(rename = "FAILED")]
    Failed,
    #[serde(rename = "CANCELLED")]
    Canceled,
}

/// Why a ticket was canceled.
#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum Reason {
    Client,
    Expired,
    Superseded,
    CacheRemoved,
    Passthrough,
}

/// Per-ref failure on the wire.
#[derive(Clone, Copy, Debug, Eq, Hash, PartialEq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum RefError {
    VersionChanged,
    NotFound,
    Forbidden,
    OriginError,
    PeerUnsupported,
    BudgetTimeout,
    InvalidRange,
}

/// Per-ref outcome on the wire.
#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum RefOutcome {
    Succeeded,
    Failed,
    Pending,
}

#[derive(Clone, Debug, Default, Eq, PartialEq, Serialize)]
pub struct Bytes {
    pub logical_requested: u64,
    pub aligned_requested: u64,
    pub already_resident: u64,
    pub queued: u64,
    pub in_flight: u64,
    pub joined: u64,
    pub fetched_from_origin: u64,
    pub admitted: u64,
    pub failed: u64,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize)]
pub struct Coverage {
    pub resident_bytes: u64,
    pub observed_at_ms: u64,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize)]
pub struct RefStatus {
    pub index: usize,
    pub key: String,
    pub etag: Option<String>,
    pub offset: u64,
    pub length: u64,
    pub outcome: RefOutcome,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub error: Option<RefError>,
}

/// `GET /v1/prefetches/{id}` and successful PUT/DELETE response.
#[derive(Clone, Debug, Eq, PartialEq, Serialize)]
pub struct Status {
    pub id: String,
    pub cache_uid: String,
    pub state: State,
    pub reason: Option<Reason>,
    pub priority: &'static str,
    pub generation: u64,
    pub sequence: u64,
    pub settled: bool,
    pub ttl_seconds: u64,
    pub expires_at_ms: u64,
    pub created_at_ms: u64,
    pub updated_at_ms: u64,
    pub refs_total: u64,
    pub refs_succeeded: u64,
    pub refs_failed: u64,
    pub refs_pending: u64,
    pub bytes: Bytes,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub coverage: Option<Coverage>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub refs: Option<Vec<RefStatus>>,
    /// Present (possibly `null`) only when `refs` is.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub next_cursor: Option<Option<String>>,
}

/// One entry of `GET /v1/prefetches`.
#[derive(Clone, Debug, Eq, PartialEq, Serialize)]
pub struct Summary {
    pub id: String,
    pub state: State,
    pub reason: Option<Reason>,
    pub priority: &'static str,
    pub generation: u64,
    pub sequence: u64,
    pub settled: bool,
    pub expires_at_ms: u64,
    pub bytes: Bytes,
}

#[derive(Serialize)]
pub struct List {
    pub prefetches: Vec<Summary>,
}

/// A refs paging cursor bound to one `(generation, sequence)`.
pub fn encode_cursor(generation: u64, sequence: u64, offset: usize) -> String {
    format!("{generation}.{sequence}.{offset}")
}

/// Decode a cursor, rejecting one from another `(generation, sequence)`.
pub fn decode_cursor(cursor: &str, generation: u64, sequence: u64) -> Result<usize, PrefetchError> {
    let mut parts = cursor.split('.');
    let mut next = || -> Option<u64> { parts.next()?.parse().ok() };
    match (next(), next(), next()) {
        (Some(g), Some(s), Some(offset)) if g == generation && s == sequence => {
            usize::try_from(offset).map_err(|_| PrefetchError::invalid("invalid cursor"))
        }
        (Some(_), Some(_), Some(_)) => Err(PrefetchError::invalid(
            "cursor belongs to another generation or sequence",
        )),
        _ => Err(PrefetchError::invalid("invalid cursor")),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    const KEY: &str = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08";

    fn parse(body: &str) -> Result<Spec, PrefetchError> {
        parse_spec(body.as_bytes(), false, &Config::default())
    }

    #[test]
    fn defaults_and_caps_apply() {
        let spec = parse(&format!(r#"{{"refs":[{{"key":"{KEY}"}}]}}"#)).unwrap();
        assert_eq!(spec.priority, Priority::Normal);
        assert_eq!((spec.generation, spec.sequence), (0, 0));
        assert_eq!(spec.ttl_seconds, 600);
        assert_eq!(spec.max_pending_bytes, 1 << 30);
        assert_eq!(spec.refs[0].offset, 0);
        assert_eq!(spec.refs[0].length, None);
        let spec = parse(&format!(
            r#"{{"priority":"high","generation":2,"sequence":5,"ttl_seconds":999999,
                "max_pending_bytes":99999999999999999,"cache_uid":"u",
                "refs":[{{"key":"{KEY}","etag":"\"v1\"","offset":7,"length":9,"metadata":"m"}}]}}"#
        ))
        .unwrap();
        assert_eq!(spec.priority, Priority::High);
        assert_eq!((spec.generation, spec.sequence), (2, 5));
        assert_eq!(spec.ttl_seconds, 3600);
        assert_eq!(spec.max_pending_bytes, 16 << 30);
        assert_eq!(spec.refs[0].etag.as_ref().unwrap().as_str(), "\"v1\"");
        assert_eq!((spec.refs[0].offset, spec.refs[0].length), (7, Some(9)));
        assert_eq!(spec.cache_uid.as_deref(), Some("u"));
    }

    #[test]
    fn strict_fields_are_rejected() {
        for body in [
            "".to_string(),
            "[]".into(),
            r#"{"refs":[]}"#.into(),
            r#"{}"#.into(),
            format!(r#"{{"refs":[{{"key":"{KEY}"}}],"extra":1}}"#),
            format!(r#"{{"refs":[{{"key":"{KEY}","extra":1}}]}}"#),
            format!(r#"{{"refs":[{{"key":"{}"}}]}}"#, KEY.to_uppercase()),
            r#"{"refs":[{"key":"abc"}]}"#.into(),
            format!(r#"{{"refs":[{{"key":"{KEY}","etag":"W/\"v\""}}]}}"#),
            format!(r#"{{"refs":[{{"key":"{KEY}","etag":"v"}}]}}"#),
            format!(r#"{{"refs":[{{"key":"{KEY}","length":0}}]}}"#),
            format!(r#"{{"refs":[{{"key":"{KEY}","offset":-1}}]}}"#),
            format!(r#"{{"refs":[{{"key":"{KEY}","metadata":" padded "}}]}}"#),
            format!(r#"{{"priority":"urgent","refs":[{{"key":"{KEY}"}}]}}"#),
            format!(r#"{{"generation":9007199254740992,"refs":[{{"key":"{KEY}"}}]}}"#),
            format!(r#"{{"sequence":9007199254740992,"refs":[{{"key":"{KEY}"}}]}}"#),
            format!(r#"{{"ttl_seconds":0,"refs":[{{"key":"{KEY}"}}]}}"#),
            format!(r#"{{"generation":1.5,"refs":[{{"key":"{KEY}"}}]}}"#),
        ] {
            let error = parse(&body).unwrap_err();
            assert_eq!(error.code, ErrorCode::InvalidRequest, "{body}");
        }
        assert!(
            parse(&format!(
                r#"{{"generation":9007199254740991,"refs":[{{"key":"{KEY}"}}]}}"#
            ))
            .is_ok()
        );
    }

    #[test]
    fn too_many_refs_is_too_large() {
        let config = Config {
            max_refs: 2,
            ..Config::default()
        };
        let body = format!(r#"{{"refs":[{{"key":"{KEY}"}},{{"key":"{KEY}"}},{{"key":"{KEY}"}}]}}"#);
        let error = parse_spec(body.as_bytes(), false, &config).unwrap_err();
        assert_eq!(error.code, ErrorCode::TooLarge);
        // Duplicate refs are allowed.
        let body = format!(r#"{{"refs":[{{"key":"{KEY}"}},{{"key":"{KEY}"}}]}}"#);
        assert_eq!(
            parse_spec(body.as_bytes(), false, &config)
                .unwrap()
                .refs
                .len(),
            2
        );
    }

    #[test]
    fn hash_covers_canonical_body_and_authorization_presence() {
        let a = parse(&format!(r#"{{"refs":[{{"key":"{KEY}"}}]}}"#)).unwrap();
        let b = parse(&format!("{{ \"refs\" : [ {{ \"key\" : \"{KEY}\" }} ] }}")).unwrap();
        assert_eq!(a.hash, b.hash, "whitespace is not significant");
        let c = parse(&format!(r#"{{"refs":[{{"key":"{KEY}","offset":1}}]}}"#)).unwrap();
        assert_ne!(a.hash, c.hash);
        let d = parse_spec(
            format!(r#"{{"refs":[{{"key":"{KEY}"}}]}}"#).as_bytes(),
            true,
            &Config::default(),
        )
        .unwrap();
        assert_ne!(a.hash, d.hash);
    }

    #[test]
    fn cursors_bind_generation_and_sequence() {
        let cursor = encode_cursor(2, 5, 256);
        assert_eq!(decode_cursor(&cursor, 2, 5).unwrap(), 256);
        assert!(decode_cursor(&cursor, 2, 6).is_err());
        assert!(decode_cursor(&cursor, 3, 5).is_err());
        assert!(decode_cursor("x", 2, 5).is_err());
    }

    #[test]
    fn status_serializes_optional_sections_only_when_requested() {
        let status = Status {
            id: "a".into(),
            cache_uid: "u".into(),
            state: State::Partial,
            reason: None,
            priority: "normal",
            generation: 0,
            sequence: 0,
            settled: true,
            ttl_seconds: 600,
            expires_at_ms: 1,
            created_at_ms: 1,
            updated_at_ms: 1,
            refs_total: 1,
            refs_succeeded: 0,
            refs_failed: 1,
            refs_pending: 0,
            bytes: Bytes::default(),
            coverage: None,
            refs: None,
            next_cursor: None,
        };
        let json = serde_json::to_string(&status).unwrap();
        assert!(json.contains(r#""state":"PARTIAL""#));
        assert!(json.contains(r#""reason":null"#));
        assert!(!json.contains("coverage"));
        assert!(!json.contains("next_cursor"));
        let mut with_refs = status.clone();
        with_refs.refs = Some(vec![RefStatus {
            index: 1,
            key: KEY.into(),
            etag: None,
            offset: 0,
            length: 1,
            outcome: RefOutcome::Failed,
            error: Some(RefError::VersionChanged),
        }]);
        with_refs.next_cursor = Some(None);
        with_refs.reason = Some(Reason::CacheRemoved);
        let json = serde_json::to_string(&with_refs).unwrap();
        assert!(json.contains(r#""next_cursor":null"#));
        assert!(json.contains(r#""error":"version_changed""#));
        assert!(json.contains(r#""etag":null"#));
        assert!(json.contains(r#""reason":"cache_removed""#));
    }
}
