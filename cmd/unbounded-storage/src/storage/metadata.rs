// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Object metadata payload: the on-page format of an object's
//! dedicated metadata entry.
//!
//! A metadata entry is a content-addressed cache entry (see
//! [`OriginRef::metadata_entry`](crate::storage::OriginRef::metadata_entry))
//! whose page does not hold object data. Instead it carries
//! [`ObjectMetadata`]: the object's byte length plus a small set of
//! opaque key/value pairs passed through the system between a backend
//! (writer) and a frontend (reader). Every other layer (bufferpool,
//! btree, p2p, fabric) treats the page as opaque bytes; only the
//! origin-facing backends and the client-facing frontends interpret it
//! through this codec.
//!
//! ## On-page layout
//!
//! ```text
//! [0..8)            u64 little-endian  blob_len
//! [8..8+blob_len)   bincode(ObjectMetadata)
//! [8+blob_len..)    zero padding to the end of the page
//! ```
//!
//! The explicit `blob_len` prefix lets [`ObjectMetadata::decode`]
//! deserialize an exact sub-slice without depending on bincode's
//! trailing-byte policy, so the zero padding a backend writes after the
//! payload is ignored. The only size limit on the encoded metadata is
//! the cache page itself: the backend write path rejects a payload that
//! does not fit in the entry's page.

use std::collections::BTreeMap;
use std::sync::LazyLock;
use std::sync::atomic::{AtomicU64, Ordering};
use std::time::{SystemTime, UNIX_EPOCH};

use serde::{Deserialize, Serialize};

use crate::bufferpool::Error;

/// Width of the little-endian `blob_len` prefix that precedes the
/// bincode payload on the page.
const BLOB_LEN_PREFIX: usize = 8;
const ENTRY_ETAG: &str = "etag";
const ENTRY_CACHE_KEY_VERSION: &str = "cache-key-version";
const ENTRY_CACHE_TTL_MS: &str = "cache-ttl-ms";
const ENTRY_FETCHED_AT_UNIX_MS: &str = "fetched-at-unix-ms";

static UNVALIDATED_CACHE_GENERATION: AtomicU64 = AtomicU64::new(1);
static UNVALIDATED_CACHE_SALT: LazyLock<u64> = LazyLock::new(rand::random::<u64>);

/// An object's metadata: its byte length plus a small set of opaque
/// key/value pairs.
///
/// The key/value pairs are passed through unchanged between a backend
/// and a frontend; this crate attaches no semantics to them and does
/// not index them. Entries are held in a [`BTreeMap`] so the encoded
/// bytes are deterministic for a given logical set, which keeps the
/// content-addressed entry stable.
#[derive(Clone, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
pub struct ObjectMetadata {
    pub length: u64,
    pub entries: BTreeMap<String, String>,
}

impl ObjectMetadata {
    /// Construct metadata for an object of `length` bytes with no
    /// key/value pairs.
    pub fn new(length: u64) -> Self {
        Self {
            length,
            entries: BTreeMap::new(),
        }
    }

    /// Insert a key/value pair, returning the previous value for the
    /// key if one was present.
    pub fn insert(&mut self, key: impl Into<String>, value: impl Into<String>) -> Option<String> {
        self.entries.insert(key.into(), value.into())
    }

    /// Look up the value for `key`, if present.
    pub fn get(&self, key: &str) -> Option<&str> {
        self.entries.get(key).map(String::as_str)
    }

    /// Iterate the key/value pairs in deterministic (sorted-key) order.
    pub fn iter(&self) -> impl Iterator<Item = (&str, &str)> {
        self.entries.iter().map(|(k, v)| (k.as_str(), v.as_str()))
    }

    /// Whether there are no key/value pairs.
    pub fn is_empty(&self) -> bool {
        self.entries.is_empty()
    }

    /// Entity tag returned by the origin metadata `HEAD`, if present.
    pub fn etag(&self) -> Option<&str> {
        self.get(ENTRY_ETAG)
    }

    /// Store a strong origin entity tag exactly as returned in the header.
    pub fn set_etag(&mut self, etag: impl Into<String>) {
        self.insert(ENTRY_ETAG, etag);
    }

    /// Local body-cache namespace used when the origin requires
    /// revalidation but did not provide a strong validator.
    pub fn local_cache_key_version(&self) -> Option<&str> {
        self.get(ENTRY_CACHE_KEY_VERSION)
    }

    /// Store a local body-cache namespace.
    pub fn set_local_cache_key_version(&mut self, version: impl Into<String>) {
        self.insert(ENTRY_CACHE_KEY_VERSION, version);
    }

    /// Cache freshness TTL in milliseconds, if the backend supplied one.
    pub fn cache_ttl_ms(&self) -> Option<u64> {
        self.get(ENTRY_CACHE_TTL_MS)?.parse().ok()
    }

    /// Store the backend-supplied cache freshness TTL in milliseconds.
    pub fn set_cache_ttl_ms(&mut self, ttl_ms: u64) {
        self.insert(ENTRY_CACHE_TTL_MS, ttl_ms.to_string());
    }

    /// Unix timestamp in milliseconds when this metadata was fetched.
    pub fn fetched_at_unix_ms(&self) -> Option<u64> {
        self.get(ENTRY_FETCHED_AT_UNIX_MS)?.parse().ok()
    }

    /// Store the Unix timestamp in milliseconds when this metadata was fetched.
    pub fn set_fetched_at_unix_ms(&mut self, fetched_at_ms: u64) {
        self.insert(ENTRY_FETCHED_AT_UNIX_MS, fetched_at_ms.to_string());
    }

    /// Whether this cached metadata entry is still fresh at `now_ms`.
    /// Entries without a backend TTL are immutable from the cache's point
    /// of view and never require revalidation.
    pub fn is_fresh_at(&self, now_ms: u64) -> bool {
        let Some(ttl_ms) = self.cache_ttl_ms() else {
            return true;
        };
        let Some(fetched_at_ms) = self.fetched_at_unix_ms() else {
            return false;
        };
        now_ms < fetched_at_ms.saturating_add(ttl_ms)
    }

    /// The version string folded into data stripe keys. Strong ETags are
    /// preferred because they can also validate origin GETs; for
    /// cache-controlled responses without a strong ETag, this falls back
    /// to a local generation so refreshed metadata cannot address old
    /// body stripes.
    pub fn cache_key_version(&self) -> Option<&str> {
        self.etag().or_else(|| self.local_cache_key_version())
    }

    /// Strong origin validator safe to send as `If-Match` on data GETs.
    pub fn origin_match_version(&self) -> Option<&str> {
        self.etag()
    }

    /// Build metadata from an origin `HEAD` response. Backends express
    /// per-key TTLs through the standard `Cache-Control` header. Strong
    /// ETags are used as origin validators; without one, cache-controlled
    /// responses receive a local body namespace so revalidation never
    /// reuses stripes fetched for an older metadata observation.
    pub fn from_origin_head(
        length: u64,
        etag: Option<&str>,
        cache_control: Option<&str>,
        fetched_at_ms: u64,
    ) -> Self {
        let mut meta = Self::new(length);
        let ttl_ms = cache_control.and_then(cache_control_ttl_ms);
        if let Some(etag) = etag.filter(|v| is_strong_etag(v)) {
            meta.set_etag(etag);
        }
        if let Some(ttl_ms) = ttl_ms {
            meta.set_cache_ttl_ms(ttl_ms);
            meta.set_fetched_at_unix_ms(fetched_at_ms);
            if meta.etag().is_none() {
                meta.set_local_cache_key_version(next_unvalidated_cache_version(fetched_at_ms));
            }
        }
        meta
    }

    /// Encode into the on-page byte layout: an 8-byte little-endian
    /// payload-length prefix followed by the bincode payload. The
    /// caller writes this into the metadata entry's page (zero-filling
    /// any remainder); the page-capacity check at the write edge is the
    /// only size limit on the result.
    pub fn encode(&self) -> Result<Vec<u8>, Error> {
        let blob = bincode::serialize(self)
            .map_err(|_| Error::from("storage metadata: bincode serialize failed"))?;
        let mut out = Vec::with_capacity(BLOB_LEN_PREFIX + blob.len());
        out.extend_from_slice(&(blob.len() as u64).to_le_bytes());
        out.extend_from_slice(&blob);
        Ok(out)
    }

    /// Decode from a metadata entry's page. Reads the `blob_len` prefix,
    /// bounds-checks it against the page, and deserializes exactly that
    /// many payload bytes, ignoring any trailing zero padding.
    pub fn decode(page: &[u8]) -> Result<ObjectMetadata, Error> {
        if page.len() < BLOB_LEN_PREFIX {
            return Err(Error::from(
                "storage metadata: page smaller than length prefix",
            ));
        }
        let blob_len = u64::from_le_bytes(page[..BLOB_LEN_PREFIX].try_into().unwrap()) as usize;
        let end = BLOB_LEN_PREFIX
            .checked_add(blob_len)
            .ok_or_else(|| Error::from("storage metadata: blob length overflow"))?;
        if end > page.len() {
            return Err(Error::from("storage metadata: blob length exceeds page"));
        }
        bincode::deserialize(&page[BLOB_LEN_PREFIX..end])
            .map_err(|_| Error::from("storage metadata: malformed bincode payload"))
    }
}

/// Current Unix timestamp in milliseconds for metadata freshness stamps.
pub fn now_unix_millis() -> u64 {
    let millis = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap_or_default()
        .as_millis();
    millis.min(u128::from(u64::MAX)) as u64
}

fn cache_control_ttl_ms(value: &str) -> Option<u64> {
    let mut max_age = None;
    let mut shared_max_age = None;
    for directive in value.split(',') {
        let directive = directive.trim();
        let (name, raw_value) = directive
            .split_once('=')
            .map_or((directive, None), |(name, value)| (name, Some(value)));
        let name = name.trim();
        if name.eq_ignore_ascii_case("no-cache") || name.eq_ignore_ascii_case("no-store") {
            return Some(0);
        }
        let Some(raw_value) = raw_value else {
            continue;
        };
        let Ok(seconds) = raw_value.trim().trim_matches('"').parse::<u64>() else {
            continue;
        };
        let ttl_ms = seconds.saturating_mul(1000);
        if name.eq_ignore_ascii_case("s-maxage") {
            shared_max_age = Some(ttl_ms);
        } else if name.eq_ignore_ascii_case("max-age") {
            max_age = Some(ttl_ms);
        }
    }
    shared_max_age.or(max_age)
}

fn is_strong_etag(value: &str) -> bool {
    !value.is_empty() && !value.starts_with("W/")
}

fn next_unvalidated_cache_version(fetched_at_ms: u64) -> String {
    let generation = UNVALIDATED_CACHE_GENERATION.fetch_add(1, Ordering::Relaxed);
    format!(
        "unvalidated:{:016x}:{fetched_at_ms}:{generation}",
        *UNVALIDATED_CACHE_SALT
    )
}

#[cfg(test)]
mod tests {
    use super::*;

    fn with_entries() -> ObjectMetadata {
        let mut m = ObjectMetadata::new(4096);
        m.insert("content-type", "application/octet-stream");
        m.insert("etag", "\"abc123\"");
        m
    }

    #[test]
    fn round_trips_empty() {
        let m = ObjectMetadata::new(0);
        let decoded = ObjectMetadata::decode(&m.encode().unwrap()).unwrap();
        assert_eq!(decoded, m);
        assert_eq!(decoded.length, 0);
        assert!(decoded.is_empty());
    }

    #[test]
    fn round_trips_with_entries() {
        let m = with_entries();
        let decoded = ObjectMetadata::decode(&m.encode().unwrap()).unwrap();
        assert_eq!(decoded, m);
        assert_eq!(decoded.length, 4096);
        assert_eq!(
            decoded.get("content-type"),
            Some("application/octet-stream")
        );
        assert_eq!(decoded.get("etag"), Some("\"abc123\""));
        assert_eq!(decoded.get("missing"), None);
    }

    #[test]
    fn origin_head_stores_etag_ttl_and_fetch_time() {
        let meta = ObjectMetadata::from_origin_head(
            123,
            Some("\"abc\""),
            Some("public, max-age=60"),
            1_000,
        );
        assert_eq!(meta.length, 123);
        assert_eq!(meta.etag(), Some("\"abc\""));
        assert_eq!(meta.cache_ttl_ms(), Some(60_000));
        assert_eq!(meta.fetched_at_unix_ms(), Some(1_000));
        assert_eq!(meta.cache_key_version(), Some("\"abc\""));
        assert_eq!(meta.origin_match_version(), Some("\"abc\""));
        assert!(meta.is_fresh_at(60_999));
        assert!(!meta.is_fresh_at(61_000));
    }

    #[test]
    fn origin_head_prefers_shared_max_age() {
        let meta =
            ObjectMetadata::from_origin_head(1, Some("etag"), Some("max-age=60, s-maxage=5"), 10);
        assert_eq!(meta.cache_ttl_ms(), Some(5_000));
    }

    #[test]
    fn origin_head_treats_no_cache_as_immediately_stale() {
        let meta = ObjectMetadata::from_origin_head(
            1,
            Some("etag"),
            Some("public, max-age=60, no-cache"),
            10,
        );
        assert_eq!(meta.cache_ttl_ms(), Some(0));
        assert_eq!(meta.fetched_at_unix_ms(), Some(10));
        assert!(!meta.is_fresh_at(10));
    }

    #[test]
    fn origin_head_treats_no_store_as_immediately_stale() {
        let meta =
            ObjectMetadata::from_origin_head(1, Some("etag"), Some("s-maxage=60, no-store"), 10);
        assert_eq!(meta.cache_ttl_ms(), Some(0));
        assert_eq!(meta.fetched_at_unix_ms(), Some(10));
        assert!(!meta.is_fresh_at(10));
    }

    #[test]
    fn origin_head_uses_local_cache_namespace_without_etag() {
        let meta = ObjectMetadata::from_origin_head(1, None, Some("max-age=5"), 10);
        assert_eq!(meta.etag(), None);
        assert_eq!(meta.cache_ttl_ms(), Some(5_000));
        assert_eq!(meta.fetched_at_unix_ms(), Some(10));
        assert_eq!(meta.cache_key_version(), meta.local_cache_key_version());
        assert!(meta.cache_key_version().is_some());
        assert_eq!(meta.origin_match_version(), None);
        assert!(meta.is_fresh_at(5_009));
        assert!(!meta.is_fresh_at(5_010));
    }

    #[test]
    fn ttl_without_etag_changes_body_cache_namespace() {
        let a = ObjectMetadata::from_origin_head(1, None, Some("max-age=5"), 10);
        let b = ObjectMetadata::from_origin_head(1, None, Some("max-age=5"), 10);

        assert_ne!(a.cache_key_version(), None);
        assert_ne!(a.cache_key_version(), b.cache_key_version());
        assert_eq!(a.origin_match_version(), None);
        assert_eq!(b.origin_match_version(), None);
    }

    #[test]
    fn origin_head_honors_immediate_revalidation_without_etag() {
        let meta = ObjectMetadata::from_origin_head(1, None, Some("no-cache"), 10);
        assert_eq!(meta.etag(), None);
        assert_eq!(meta.cache_ttl_ms(), Some(0));
        assert_eq!(meta.fetched_at_unix_ms(), Some(10));
        assert!(meta.cache_key_version().is_some());
        assert_eq!(meta.origin_match_version(), None);
        assert!(!meta.is_fresh_at(10));
    }

    #[test]
    fn immediate_revalidation_without_etag_changes_body_cache_namespace() {
        let a = ObjectMetadata::from_origin_head(1, None, Some("no-cache"), 10);
        let b = ObjectMetadata::from_origin_head(1, None, Some("no-cache"), 10);

        assert_ne!(a.cache_key_version(), None);
        assert_ne!(a.cache_key_version(), b.cache_key_version());
        assert_eq!(a.origin_match_version(), None);
        assert_eq!(b.origin_match_version(), None);
    }

    #[test]
    fn weak_etag_with_ttl_gets_local_cache_namespace() {
        let meta = ObjectMetadata::from_origin_head(1, Some("W/\"abc\""), Some("max-age=5"), 10);
        assert_eq!(meta.etag(), None);
        assert_eq!(meta.cache_ttl_ms(), Some(5_000));
        assert_eq!(meta.fetched_at_unix_ms(), Some(10));
        assert_eq!(meta.cache_key_version(), meta.local_cache_key_version());
        assert!(meta.cache_key_version().is_some());
        assert_eq!(meta.origin_match_version(), None);
        assert!(meta.is_fresh_at(5_009));
        assert!(!meta.is_fresh_at(5_010));
    }

    #[test]
    fn weak_etag_with_immediate_revalidation_gets_local_cache_namespace() {
        let meta = ObjectMetadata::from_origin_head(1, Some("W/\"abc\""), Some("no-store"), 10);
        assert_eq!(meta.etag(), None);
        assert_eq!(meta.cache_ttl_ms(), Some(0));
        assert!(meta.cache_key_version().is_some());
        assert_eq!(meta.origin_match_version(), None);
    }

    #[test]
    fn encoding_is_deterministic_regardless_of_insertion_order() {
        let mut a = ObjectMetadata::new(7);
        a.insert("b", "2");
        a.insert("a", "1");

        let mut b = ObjectMetadata::new(7);
        b.insert("a", "1");
        b.insert("b", "2");

        assert_eq!(a.encode().unwrap(), b.encode().unwrap());
    }

    #[test]
    fn iter_yields_sorted_keys() {
        let mut m = ObjectMetadata::new(1);
        m.insert("zeta", "z");
        m.insert("alpha", "a");
        m.insert("mu", "m");
        let keys: Vec<&str> = m.iter().map(|(k, _)| k).collect();
        assert_eq!(keys, ["alpha", "mu", "zeta"]);
    }

    #[test]
    fn decode_ignores_trailing_page_padding() {
        // Simulate a real cache page: payload followed by zero fill.
        let m = with_entries();
        let mut page = m.encode().unwrap();
        page.resize(4096, 0);
        let decoded = ObjectMetadata::decode(&page).unwrap();
        assert_eq!(decoded, m);
    }

    #[test]
    fn decode_rejects_page_shorter_than_prefix() {
        assert!(ObjectMetadata::decode(&[0u8; 4]).is_err());
        assert!(ObjectMetadata::decode(&[]).is_err());
    }

    #[test]
    fn decode_rejects_blob_len_exceeding_page() {
        // Prefix claims 100 payload bytes but the page has only a few.
        let mut page = Vec::new();
        page.extend_from_slice(&100u64.to_le_bytes());
        page.extend_from_slice(&[0u8; 8]);
        assert!(ObjectMetadata::decode(&page).is_err());
    }

    #[test]
    fn decode_rejects_malformed_payload() {
        // Valid prefix, but the payload is not a valid bincode encoding
        // of ObjectMetadata (a u64 length followed by a map count that
        // overruns the available bytes).
        let mut page = Vec::new();
        let garbage = [0xffu8; 16];
        page.extend_from_slice(&(garbage.len() as u64).to_le_bytes());
        page.extend_from_slice(&garbage);
        assert!(ObjectMetadata::decode(&page).is_err());
    }

    #[test]
    fn large_metadata_that_fits_round_trips() {
        let mut m = ObjectMetadata::new(u64::MAX);
        for i in 0..64 {
            m.insert(format!("key-{i:04}"), format!("value-{i:08}"));
        }
        let encoded = m.encode().unwrap();
        let decoded = ObjectMetadata::decode(&encoded).unwrap();
        assert_eq!(decoded, m);
        assert_eq!(decoded.entries.len(), 64);
    }
}
