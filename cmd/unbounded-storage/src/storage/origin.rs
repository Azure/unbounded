// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Stripe-to-origin mapping.
//!
//! The P2P cache is content-addressed: a stripe is identified solely
//! by its 32-byte [`StripeKey`](crate::bufferpool::StripeKey). That is
//! sufficient for peer routing and dedup, but it carries no
//! information about *where the bytes came from*. When a read misses
//! all the way through to the origin tier, the backend needs to map a
//! [`StripeKey`] back to a concrete origin object and byte range so it
//! can issue (for example) an S3 `GET` with the right `Range` header.
//!
//! Rather than maintain a reverse sidecar from key back to origin, the
//! origin coordinates ride on the request itself ([`OriginRef`] on
//! [`StripeReq`]) from the frontend all the way to the origin tier, so
//! the backend reconstructs the byte range directly without any lookup.
//!
//! This module defines the canonical contract both the future S3
//! backend and S3 frontend bind to:
//!
//! - [`OriginRef`] names the origin object a stripe belongs to and the
//!   stripe index within that object.
//! - [`OriginRef::stripe_key`] derives the content-addressed
//!   [`StripeKey`] from an [`OriginRef`] deterministically, so the
//!   frontend (which knows the origin) and any peer (which only knows
//!   the key) agree on routing without coordination.

use serde::{Deserialize, Serialize};

use crate::bufferpool::{Error, Req, StripeKey};

use super::metadata::{ObjectMetadata, now_unix_millis};

/// Reserved sentinel `stripe_idx` for an object's metadata entry.
///
/// A metadata entry is a dedicated content-addressed entry that stores
/// the object's [`ObjectMetadata`](crate::storage::ObjectMetadata) (its
/// byte length plus a small key/value set) instead of object data. It
/// reuses the same key machinery as data stripes, identified by this
/// sentinel index.
///
/// A real data stripe is `byte_offset / stripe_size_bytes` and can
/// never reach `u64::MAX` (an object cannot have `2^64` stripes), so
/// there is no collision between a metadata entry and any data stripe.
///
/// The backend recognizes this sentinel and fills the entry by issuing
/// a `HEAD` against the origin to learn its length, rather than doing a
/// ranged `GET` for object bytes.
pub const METADATA_STRIPE_IDX: u64 = u64::MAX;

/// Identifies the origin object a stripe belongs to.
///
/// `backend_id` selects one of the node's configured backends (an S3
/// endpoint + bucket + credentials, an OCI registry, ...).
/// `origin_object_id` is the object's key/name within that backend
/// (an S3 object key, a blob digest, ...). `stripe_idx` is the
/// zero-based index of the stripe within the object, computed by the
/// frontend as `byte_offset / stripe_size_bytes`.
#[derive(Clone, Debug, PartialEq, Eq, Hash, Serialize, Deserialize)]
pub struct OriginRef {
    pub backend_id: String,
    pub origin_object_id: String,
    pub stripe_idx: u64,
    pub cache_key_version: Option<String>,
    pub origin_match_version: Option<String>,
}

impl OriginRef {
    pub fn new(
        backend_id: impl Into<String>,
        origin_object_id: impl Into<String>,
        stripe_idx: u64,
    ) -> Self {
        Self {
            backend_id: backend_id.into(),
            origin_object_id: origin_object_id.into(),
            stripe_idx,
            cache_key_version: None,
            origin_match_version: None,
        }
    }

    /// Construct an [`OriginRef`] naming the object's metadata entry: a
    /// dedicated content-addressed entry that stores the object's
    /// [`ObjectMetadata`](crate::storage::ObjectMetadata) rather than
    /// data, identified by the reserved sentinel
    /// [`METADATA_STRIPE_IDX`].
    pub fn metadata_entry(
        backend_id: impl Into<String>,
        origin_object_id: impl Into<String>,
    ) -> Self {
        Self {
            backend_id: backend_id.into(),
            origin_object_id: origin_object_id.into(),
            stripe_idx: METADATA_STRIPE_IDX,
            cache_key_version: None,
            origin_match_version: None,
        }
    }

    /// Return a copy whose cache key includes `version`. The origin path
    /// stays unchanged; only the content-addressed cache identity changes.
    pub fn with_cache_key_version(mut self, version: impl Into<String>) -> Self {
        let version = version.into();
        if !version.is_empty() {
            self.cache_key_version = Some(version);
        }
        self
    }

    /// Return a copy whose origin GETs are conditioned on `version`.
    /// This must only carry strong origin validators, never local cache
    /// generations.
    pub fn with_origin_match_version(mut self, version: impl Into<String>) -> Self {
        let version = version.into();
        if !version.is_empty() {
            self.origin_match_version = Some(version);
        }
        self
    }

    /// Derive the canonical content-addressed [`StripeKey`] for this
    /// origin stripe.
    ///
    /// The key is `blake3(...)` over the following exact byte layout,
    /// fed to a single streaming hasher in this order. Each
    /// variable-length string field is length-prefixed with its byte
    /// length as a little-endian `u64`, so distinct `(backend_id,
    /// origin_object_id)` splits can never alias:
    ///
    /// 1. `backend_id.len()` as 8 little-endian bytes
    ///    (`u64::to_le_bytes`).
    /// 2. `backend_id` as raw UTF-8 bytes.
    /// 3. `origin_object_id.len()` as 8 little-endian bytes.
    /// 4. `origin_object_id` as raw UTF-8 bytes.
    /// 5. `stripe_idx` as 8 little-endian bytes (`u64::to_le_bytes`).
    /// 6. If present, a one-byte version marker, then
    ///    `cache_key_version.len()` as 8 little-endian bytes, then
    ///    `cache_key_version` as raw UTF-8 bytes.
    ///
    /// The 32-byte blake3 digest is the [`StripeKey`]. blake3's output
    /// is already 32 bytes, so no truncation or expansion is needed.
    ///
    /// The length prefix on each variable-length field makes the
    /// encoding unambiguous: `("ab", "c")` and `("a", "bc")` frame to
    /// different byte streams and therefore different keys, so the
    /// split between `backend_id` and `origin_object_id` need not be
    /// pinned out-of-band for correctness. The 8-byte little-endian
    /// `stripe_idx` suffix is fixed width.
    pub fn stripe_key(&self) -> StripeKey {
        stripe_key_versioned(
            &self.backend_id,
            &self.origin_object_id,
            self.stripe_idx,
            self.cache_key_version.as_deref(),
        )
    }

    /// Whether this reference names an object's metadata entry rather
    /// than a data stripe, i.e. its `stripe_idx` is the reserved
    /// sentinel [`METADATA_STRIPE_IDX`].
    pub fn is_metadata_entry(&self) -> bool {
        self.stripe_idx == METADATA_STRIPE_IDX
    }
}

/// Request type for the shard buffer [`Pool`](crate::bufferpool::Pool).
///
/// `key` is the content-addressed [`StripeKey`] that routes and
/// identifies the stripe throughout the cache and peer fabric; it is
/// the only field peer handlers ever look at. `origin` is the optional
/// stripe-to-origin mapping consulted *only* at the origin tier (the
/// HTTP backend) to translate the stripe into an origin URL-path byte
/// range when a read misses all the way through. Peers that merely
/// serve cached pages ignore `origin` entirely.
///
/// The type is `Serialize + Deserialize` because the fabric transport
/// carries requests over the wire, and `Clone` because the pool may
/// retain a copy while a fetch is in flight.
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
pub struct StripeReq {
    key: StripeKey,
    origin: Option<OriginRef>,
    force_refresh_cached_page: bool,
}

impl StripeReq {
    /// Construct a request that carries only the routing key, with no
    /// origin mapping attached.
    pub fn new(key: StripeKey) -> Self {
        Self {
            key,
            origin: None,
            force_refresh_cached_page: false,
        }
    }

    /// Attach the origin mapping the HTTP backend uses to resolve a
    /// miss to an origin byte range, returning the updated request.
    pub fn with_origin(mut self, origin: OriginRef) -> Self {
        self.origin = Some(origin);
        self
    }

    /// Force a cached metadata entry to be re-read from its backend even
    /// if its TTL still marks it fresh.
    pub fn with_force_refresh_cached_page(mut self) -> Self {
        self.force_refresh_cached_page = true;
        self
    }

    /// The origin mapping, if one was attached. `None` for requests
    /// that never reach the origin tier.
    pub fn origin(&self) -> Option<&OriginRef> {
        self.origin.as_ref()
    }
}

impl Req for StripeReq {
    fn key(&self) -> StripeKey {
        self.key
    }

    fn should_refresh_cached_page(&self, page: &[u8]) -> Result<bool, Error> {
        let Some(origin) = self.origin() else {
            return Ok(false);
        };
        if !origin.is_metadata_entry() {
            return Ok(false);
        }
        if self.force_refresh_cached_page {
            return Ok(true);
        }
        let Ok(meta) = ObjectMetadata::decode(page) else {
            return Ok(true);
        };
        Ok(!meta.is_fresh_at(now_unix_millis()))
    }
}

/// Free-function form of [`OriginRef::stripe_key`], for callers that
/// have the parts in hand without an `OriginRef` value. See
/// [`OriginRef::stripe_key`] for the exact byte layout.
pub fn stripe_key(backend_id: &str, origin_object_id: &str, stripe_idx: u64) -> StripeKey {
    stripe_key_versioned(backend_id, origin_object_id, stripe_idx, None)
}

fn stripe_key_versioned(
    backend_id: &str,
    origin_object_id: &str,
    stripe_idx: u64,
    cache_key_version: Option<&str>,
) -> StripeKey {
    let mut hasher = blake3::Hasher::new();
    hasher.update(&(backend_id.len() as u64).to_le_bytes());
    hasher.update(backend_id.as_bytes());
    hasher.update(&(origin_object_id.len() as u64).to_le_bytes());
    hasher.update(origin_object_id.as_bytes());
    hasher.update(&stripe_idx.to_le_bytes());
    if let Some(version) = cache_key_version {
        hasher.update(&[1]);
        hasher.update(&(version.len() as u64).to_le_bytes());
        hasher.update(version.as_bytes());
    }
    StripeKey(*hasher.finalize().as_bytes())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn stripe_key_is_deterministic() {
        let a = OriginRef::new("primary-s3", "models/llama.bin", 12);
        let b = OriginRef::new("primary-s3", "models/llama.bin", 12);
        assert_eq!(a.stripe_key(), b.stripe_key());
        // Free function agrees with the method.
        assert_eq!(
            a.stripe_key(),
            stripe_key("primary-s3", "models/llama.bin", 12)
        );
    }

    #[test]
    fn stripe_key_differs_on_any_field() {
        let base = OriginRef::new("primary-s3", "models/llama.bin", 12);
        let diff_backend = OriginRef::new("secondary-s3", "models/llama.bin", 12);
        let diff_object = OriginRef::new("primary-s3", "models/other.bin", 12);
        let diff_idx = OriginRef::new("primary-s3", "models/llama.bin", 13);

        let k = base.stripe_key();
        assert_ne!(k, diff_backend.stripe_key());
        assert_ne!(k, diff_object.stripe_key());
        assert_ne!(k, diff_idx.stripe_key());
    }

    #[test]
    fn stripe_key_matches_documented_byte_layout() {
        // Reconstruct the digest by hand from the documented layout:
        // backend_len_le || backend_id bytes || object_len_le ||
        // origin_object_id bytes || idx_le.
        let backend = "b";
        let object = "obj";
        let idx: u64 = 0x0102_0304_0506_0708;

        let mut expected_input = Vec::new();
        expected_input.extend_from_slice(&(backend.len() as u64).to_le_bytes());
        expected_input.extend_from_slice(backend.as_bytes());
        expected_input.extend_from_slice(&(object.len() as u64).to_le_bytes());
        expected_input.extend_from_slice(object.as_bytes());
        expected_input.extend_from_slice(&idx.to_le_bytes());
        let expected = StripeKey(*blake3::hash(&expected_input).as_bytes());

        assert_eq!(stripe_key(backend, object, idx), expected);
    }

    #[test]
    fn stripe_key_field_lengths_prevent_collision() {
        // The length prefix on each variable-length field must make
        // distinct (backend_id, origin_object_id) splits unambiguous:
        // without it, "ab"+"c" and "a"+"bc" hash the same bytes.
        assert_ne!(stripe_key("ab", "c", 0), stripe_key("a", "bc", 0));

        // Moving a byte across the field boundary changes the key even
        // when the concatenation is otherwise identical.
        assert_ne!(
            stripe_key("foo", "barbaz", 7),
            stripe_key("foobar", "baz", 7)
        );

        // Empty-vs-nonempty split of the same concatenation also
        // differs.
        assert_ne!(stripe_key("", "x", 0), stripe_key("x", "", 0));
    }

    #[test]
    fn stripe_index_uses_little_endian() {
        // Index 1 must encode as 0x01,0x00,... not big-endian. Verify
        // by feeding the explicit little-endian bytes and matching.
        let mut input = Vec::new();
        input.extend_from_slice(&1u64.to_le_bytes());
        input.extend_from_slice(b"b");
        input.extend_from_slice(&3u64.to_le_bytes());
        input.extend_from_slice(b"obj");
        input.extend_from_slice(&[1, 0, 0, 0, 0, 0, 0, 0]);
        let expected = StripeKey(*blake3::hash(&input).as_bytes());
        assert_eq!(stripe_key("b", "obj", 1), expected);

        // And that little-endian 1 differs from big-endian 1 framing.
        let mut be_input = Vec::new();
        be_input.extend_from_slice(&1u64.to_le_bytes());
        be_input.extend_from_slice(b"b");
        be_input.extend_from_slice(&3u64.to_le_bytes());
        be_input.extend_from_slice(b"obj");
        be_input.extend_from_slice(&[0, 0, 0, 0, 0, 0, 0, 1]);
        let be = StripeKey(*blake3::hash(&be_input).as_bytes());
        assert_ne!(stripe_key("b", "obj", 1), be);
    }

    #[test]
    fn stripe_key_handles_edge_inputs() {
        // Empty object id and max stripe index must still produce a
        // stable, distinct key with no panic.
        let empty = OriginRef::new("", "", 0);
        let max_idx = OriginRef::new("backend", "object", u64::MAX);
        assert_eq!(empty.stripe_key(), OriginRef::new("", "", 0).stripe_key());
        assert_eq!(
            max_idx.stripe_key(),
            OriginRef::new("backend", "object", u64::MAX).stripe_key()
        );
        assert_ne!(empty.stripe_key(), max_idx.stripe_key());
    }

    #[test]
    fn metadata_entry_uses_sentinel_index() {
        let me = OriginRef::metadata_entry("b", "obj");
        assert_eq!(me.stripe_idx, METADATA_STRIPE_IDX);
        assert!(me.is_metadata_entry());
        assert!(!OriginRef::new("b", "obj", 0).is_metadata_entry());
    }

    #[test]
    fn metadata_entry_key_is_deterministic() {
        let a = OriginRef::metadata_entry("b", "obj");
        let b = OriginRef::metadata_entry("b", "obj");
        assert_eq!(a.stripe_key(), b.stripe_key());
        // The metadata entry rides the same keying machinery as a data
        // stripe at the sentinel index.
        assert_eq!(a.stripe_key(), stripe_key("b", "obj", u64::MAX));
    }

    #[test]
    fn metadata_entry_key_distinct_from_data_stripes() {
        let me = OriginRef::metadata_entry("b", "obj").stripe_key();
        for idx in [0u64, 1, 2, 1000] {
            assert_ne!(me, OriginRef::new("b", "obj", idx).stripe_key());
        }
    }

    #[test]
    fn versioned_data_key_changes_identity_without_changing_origin_path() {
        let base = OriginRef::new("b", "obj", 3);
        let v1 = base.clone().with_cache_key_version("etag-1");
        let v2 = base.clone().with_cache_key_version("etag-2");

        assert_eq!(v1.origin_object_id, "obj");
        assert_eq!(v1.stripe_idx, 3);
        assert_ne!(base.stripe_key(), v1.stripe_key());
        assert_ne!(v1.stripe_key(), v2.stripe_key());
    }

    #[test]
    fn metadata_key_is_stable_without_version() {
        let a = OriginRef::metadata_entry("b", "obj");
        let b = OriginRef::metadata_entry("b", "obj");

        assert_eq!(a.cache_key_version, None);
        assert_eq!(a.origin_match_version, None);
        assert_eq!(a.stripe_key(), b.stripe_key());
        assert_eq!(a.stripe_key(), stripe_key("b", "obj", METADATA_STRIPE_IDX));
    }

    #[test]
    fn origin_match_version_does_not_change_cache_key() {
        let base = OriginRef::new("b", "obj", 3);
        let matched = base.clone().with_origin_match_version("\"etag-1\"");

        assert_eq!(base.stripe_key(), matched.stripe_key());
        assert_eq!(matched.origin_match_version, Some("\"etag-1\"".to_string()));
    }

    #[test]
    fn unvalidated_metadata_generations_change_body_cache_keys() {
        let a = ObjectMetadata::from_origin_head(1, None, Some("no-cache"), 10);
        let b = ObjectMetadata::from_origin_head(1, None, Some("no-cache"), 10);

        let a_origin =
            OriginRef::new("b", "obj", 3).with_cache_key_version(a.cache_key_version().unwrap());
        let b_origin =
            OriginRef::new("b", "obj", 3).with_cache_key_version(b.cache_key_version().unwrap());

        assert_ne!(a_origin.stripe_key(), b_origin.stripe_key());
        assert_eq!(a.origin_match_version(), None);
        assert_eq!(b.origin_match_version(), None);
        assert_eq!(a_origin.origin_match_version, None);
        assert_eq!(b_origin.origin_match_version, None);
    }

    #[test]
    fn origin_ref_round_trips_through_bincode() {
        let r = OriginRef::new("primary-s3", "models/llama.bin", 42);
        let bytes = bincode::serialize(&r).unwrap();
        let back: OriginRef = bincode::deserialize(&bytes).unwrap();
        assert_eq!(r, back);
    }

    #[test]
    fn stripe_req_new_has_key_and_no_origin() {
        let k = OriginRef::new("primary-s3", "models/llama.bin", 12).stripe_key();
        let req = StripeReq::new(k);
        assert_eq!(req.key(), k);
        assert!(req.origin().is_none());
    }

    #[test]
    fn stripe_req_with_origin_sets_origin_and_preserves_key() {
        let origin = OriginRef::new("primary-s3", "models/llama.bin", 12);
        let k = origin.stripe_key();
        let req = StripeReq::new(k).with_origin(origin.clone());
        assert_eq!(req.key(), k);
        assert_eq!(req.origin(), Some(&origin));
    }

    #[test]
    fn forced_metadata_refresh_overrides_fresh_entry() {
        let origin = OriginRef::metadata_entry("primary-s3", "models/llama.bin");
        let page = ObjectMetadata::from_origin_head(
            123,
            Some("\"etag-1\""),
            Some("max-age=3600"),
            now_unix_millis(),
        )
        .encode()
        .unwrap();
        let req = StripeReq::new(origin.stripe_key()).with_origin(origin.clone());
        assert!(!req.should_refresh_cached_page(&page).unwrap());

        let req = StripeReq::new(origin.stripe_key())
            .with_origin(origin)
            .with_force_refresh_cached_page();
        assert!(req.should_refresh_cached_page(&page).unwrap());
    }

    #[test]
    fn stripe_req_round_trips_through_bincode() {
        // Without an origin mapping.
        let k = OriginRef::new("primary-s3", "models/llama.bin", 7).stripe_key();
        let req = StripeReq::new(k);
        let bytes = bincode::serialize(&req).unwrap();
        let back: StripeReq = bincode::deserialize(&bytes).unwrap();
        assert_eq!(req, back);
        assert!(back.origin().is_none());

        // With an origin mapping.
        let origin = OriginRef::new("primary-s3", "models/llama.bin", 7);
        let req2 = StripeReq::new(origin.stripe_key()).with_origin(origin.clone());
        let bytes2 = bincode::serialize(&req2).unwrap();
        let back2: StripeReq = bincode::deserialize(&bytes2).unwrap();
        assert_eq!(req2, back2);
        assert_eq!(back2.origin(), Some(&origin));
    }
}
