// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Stripe-to-origin mapping.
//!
//! The P2P cache is keyspace-addressed: a cached stripe is identified
//! by its cache id, logical object id, and stripe index. That key is
//! sufficient for peer routing and dedup inside one cache, but it
//! carries no information about *where the bytes came from*. When a
//! read misses all the way through to the origin tier, the backend
//! needs to map a [`StripeKey`] back to a concrete origin object and
//! byte range so it can issue (for example) an S3 `GET` with the right
//! `Range` header.
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
//! - [`OriginRef::stripe_key_for_cache`] derives the cached
//!   [`StripeKey`] from a cache id and an [`OriginRef`] deterministically,
//!   so the frontend (which knows the origin) and any peer (which only
//!   knows the key) agree on routing without coordination.

use std::time::{SystemTime, UNIX_EPOCH};

use serde::{Deserialize, Serialize};

use crate::bufferpool::{Req, StripeKey};
use crate::storage::ObjectMetadata;

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
        }
    }

    /// Derive the uncached origin [`StripeKey`] for this origin stripe.
    ///
    /// Cached requests must use [`Self::stripe_key_for_cache`] so the
    /// cache id is the only logical keyspace prefix. This method exists
    /// for direct-backend and bypass requests that never populate a
    /// cache.
    pub fn stripe_key(&self) -> StripeKey {
        origin_stripe_key(&self.backend_id, &self.origin_object_id, self.stripe_idx)
    }

    /// Derive the canonical cached [`StripeKey`] for this origin stripe
    /// within `cache_id`.
    ///
    /// The key is `blake3(...)` over the following exact byte layout,
    /// fed to a single streaming hasher in this order. Each
    /// variable-length string field is length-prefixed with its byte
    /// length as a little-endian `u64`, so distinct `(cache_id,
    /// origin_object_id)` splits can never alias:
    ///
    /// 1. `cache_id.len()` as 8 little-endian bytes
    ///    (`u64::to_le_bytes`).
    /// 2. `cache_id` as raw UTF-8 bytes.
    /// 3. `origin_object_id.len()` as 8 little-endian bytes.
    /// 4. `origin_object_id` as raw UTF-8 bytes.
    /// 5. `stripe_idx` as 8 little-endian bytes (`u64::to_le_bytes`).
    ///
    /// The 32-byte blake3 digest is the [`StripeKey`]. blake3's output
    /// is already 32 bytes, so no truncation or expansion is needed.
    ///
    /// The length prefix on each variable-length field makes the
    /// encoding unambiguous: `("ab", "c")` and `("a", "bc")` frame to
    /// different byte streams and therefore different keys, so the
    /// split between `cache_id` and `origin_object_id` need not be
    /// pinned out-of-band for correctness. The 8-byte little-endian
    /// `stripe_idx` suffix is fixed width.
    pub fn stripe_key_for_cache(&self, cache_id: &str) -> StripeKey {
        stripe_key(cache_id, &self.origin_object_id, self.stripe_idx)
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
    cache_id: Option<String>,
    /// Local-only bridge flag. Never serialized: a bypass request is
    /// served straight from the origin on this node and never crosses
    /// the fabric, so a request decoded from a peer is by definition
    /// not a bypass. `#[serde(skip)]` keeps the wire format unchanged
    /// and defaults the field to `false` on decode.
    #[serde(skip)]
    bypass: bool,
    /// Benchmark-only flag carried over the fabric. Peer handlers use it
    /// to synthesize response pages from their RPC scratch buffers so a
    /// loadgen run can measure fabric RPC/RMA capacity without disk or
    /// origin work in the server path.
    fabric_only: bool,
    /// Local-only benchmark flag. Never serialized: the initiating
    /// bufferpool skips its own disk lookup/writeback when this is set,
    /// but peer handlers still use their local NVMe-backed cache after
    /// decoding the request from the fabric. This keeps measurements
    /// focused on remote NVMe + RDMA without short-circuiting on the
    /// requester's disk.
    #[serde(skip)]
    skip_local_disk: bool,
}

impl StripeReq {
    /// Construct a request that carries only the routing key, with no
    /// origin mapping attached.
    pub fn new(key: StripeKey) -> Self {
        Self {
            key,
            origin: None,
            cache_id: None,
            bypass: false,
            fabric_only: false,
            skip_local_disk: false,
        }
    }

    /// Attach the origin mapping the HTTP backend uses to resolve a
    /// miss to an origin byte range, returning the updated request.
    pub fn with_origin(mut self, origin: OriginRef) -> Self {
        self.origin = Some(origin);
        self
    }

    /// Attach the cache namespace this request should read from and
    /// write to. `None` means the request has no local cache tier and
    /// store lookups should behave like misses.
    pub fn with_cache_id(mut self, cache_id: Option<String>) -> Self {
        self.cache_id = cache_id;
        self
    }

    /// Mark the request to bypass the p2p cache layer (disk cache,
    /// peer routing, and cross-shard fanout), bridging straight to the
    /// origin backend. See [`Req::bypass`].
    pub fn with_bypass(mut self, bypass: bool) -> Self {
        self.bypass = bypass;
        self
    }

    /// Mark the request as a synthetic fabric benchmark request.
    pub fn with_fabric_only(mut self, fabric_only: bool) -> Self {
        self.fabric_only = fabric_only;
        self
    }

    /// Mark the request to skip the initiator's local disk cache.
    pub fn with_skip_local_disk(mut self, skip_local_disk: bool) -> Self {
        self.skip_local_disk = skip_local_disk;
        self
    }

    /// The origin mapping, if one was attached. `None` for requests
    /// that never reach the origin tier.
    pub fn origin(&self) -> Option<&OriginRef> {
        self.origin.as_ref()
    }

    pub fn cache_id(&self) -> Option<&str> {
        self.cache_id.as_deref()
    }

    pub fn fabric_only(&self) -> bool {
        self.fabric_only
    }

    pub fn skip_local_disk(&self) -> bool {
        self.skip_local_disk
    }
}

impl Req for StripeReq {
    fn key(&self) -> StripeKey {
        self.key
    }

    fn bypass(&self) -> bool {
        self.bypass
    }

    fn cache_id(&self) -> Option<&String> {
        self.cache_id.as_ref()
    }

    fn fabric_only(&self) -> bool {
        self.fabric_only
    }

    fn skip_local_disk(&self) -> bool {
        self.skip_local_disk
    }

    fn cached_page_valid(&self, stripe_off: u64, page: &[u8]) -> bool {
        self.cached_page_valid_at(stripe_off, page, unix_now_secs())
    }

    fn cached_page_valid_at(&self, stripe_off: u64, page: &[u8], now_unix_secs: u64) -> bool {
        if stripe_off != 0 {
            return true;
        }
        if !self
            .origin
            .as_ref()
            .is_some_and(OriginRef::is_metadata_entry)
        {
            return true;
        }
        ObjectMetadata::decode(page)
            .map(|meta| meta.cache_valid_at(now_unix_secs))
            .unwrap_or(false)
    }
}

fn unix_now_secs() -> u64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_secs())
        .unwrap_or(0)
}

/// Free-function form of [`OriginRef::stripe_key_for_cache`], for callers that
/// have the parts in hand without an `OriginRef` value. See
/// [`OriginRef::stripe_key_for_cache`] for the exact byte layout.
pub fn stripe_key(cache_id: &str, origin_object_id: &str, stripe_idx: u64) -> StripeKey {
    let mut hasher = blake3::Hasher::new();
    hasher.update(&(cache_id.len() as u64).to_le_bytes());
    hasher.update(cache_id.as_bytes());
    hasher.update(&(origin_object_id.len() as u64).to_le_bytes());
    hasher.update(origin_object_id.as_bytes());
    hasher.update(&stripe_idx.to_le_bytes());
    StripeKey(*hasher.finalize().as_bytes())
}

fn origin_stripe_key(backend_id: &str, origin_object_id: &str, stripe_idx: u64) -> StripeKey {
    let mut hasher = blake3::Hasher::new();
    hasher.update(&(backend_id.len() as u64).to_le_bytes());
    hasher.update(backend_id.as_bytes());
    hasher.update(&(origin_object_id.len() as u64).to_le_bytes());
    hasher.update(origin_object_id.as_bytes());
    hasher.update(&stripe_idx.to_le_bytes());
    StripeKey(*hasher.finalize().as_bytes())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn stripe_key_is_deterministic() {
        let a = OriginRef::new("primary-s3", "models/llama.bin", 12);
        let b = OriginRef::new("secondary-s3", "models/llama.bin", 12);
        assert_eq!(
            a.stripe_key_for_cache("cache-a"),
            b.stripe_key_for_cache("cache-a")
        );
        // Free function agrees with the method.
        assert_eq!(
            a.stripe_key_for_cache("cache-a"),
            stripe_key("cache-a", "models/llama.bin", 12)
        );
    }

    #[test]
    fn stripe_key_differs_on_any_field() {
        let base = OriginRef::new("primary-s3", "models/llama.bin", 12);
        let diff_cache = OriginRef::new("secondary-s3", "models/llama.bin", 12);
        let diff_object = OriginRef::new("primary-s3", "models/other.bin", 12);
        let diff_idx = OriginRef::new("primary-s3", "models/llama.bin", 13);

        let k = base.stripe_key_for_cache("cache-a");
        assert_eq!(k, diff_cache.stripe_key_for_cache("cache-a"));
        assert_ne!(k, base.stripe_key_for_cache("cache-b"));
        assert_ne!(k, diff_object.stripe_key_for_cache("cache-a"));
        assert_ne!(k, diff_idx.stripe_key_for_cache("cache-a"));
    }

    #[test]
    fn stripe_key_matches_documented_byte_layout() {
        // Reconstruct the digest by hand from the documented layout:
        // cache_len_le || cache_id bytes || object_len_le ||
        // origin_object_id bytes || idx_le.
        let cache = "c";
        let object = "obj";
        let idx: u64 = 0x0102_0304_0506_0708;

        let mut expected_input = Vec::new();
        expected_input.extend_from_slice(&(cache.len() as u64).to_le_bytes());
        expected_input.extend_from_slice(cache.as_bytes());
        expected_input.extend_from_slice(&(object.len() as u64).to_le_bytes());
        expected_input.extend_from_slice(object.as_bytes());
        expected_input.extend_from_slice(&idx.to_le_bytes());
        let expected = StripeKey(*blake3::hash(&expected_input).as_bytes());

        assert_eq!(stripe_key(cache, object, idx), expected);
    }

    #[test]
    fn stripe_key_field_lengths_prevent_collision() {
        // The length prefix on each variable-length field must make
        // distinct (cache_id, origin_object_id) splits unambiguous:
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
    fn stripe_req_bypass_defaults_false_and_is_settable() {
        let k = OriginRef::new("primary-s3", "models/llama.bin", 12).stripe_key();
        // New requests do not bypass the cache.
        assert!(!StripeReq::new(k).bypass());
        // The builder flips the flag on and preserves the key/origin.
        let origin = OriginRef::new("primary-s3", "models/llama.bin", 12);
        let req = StripeReq::new(k)
            .with_origin(origin.clone())
            .with_bypass(true);
        assert!(req.bypass());
        assert_eq!(req.key(), k);
        assert_eq!(req.origin(), Some(&origin));
        // And can be cleared again.
        assert!(
            !StripeReq::new(k)
                .with_bypass(true)
                .with_bypass(false)
                .bypass()
        );
    }

    #[test]
    fn stripe_req_fabric_only_round_trips_through_bincode() {
        let origin = OriginRef::new("primary-s3", "models/llama.bin", 7);
        let req = StripeReq::new(origin.stripe_key())
            .with_origin(origin.clone())
            .with_fabric_only(true);

        let bytes = bincode::serialize(&req).unwrap();
        let back: StripeReq = bincode::deserialize(&bytes).unwrap();

        assert!(back.fabric_only());
        assert_eq!(back.origin(), Some(&origin));
    }

    #[test]
    fn stripe_req_fabric_only_defaults_false() {
        let k = OriginRef::new("primary-s3", "models/llama.bin", 12).stripe_key();

        assert!(!StripeReq::new(k).fabric_only());
        assert!(!StripeReq::new(k).skip_local_disk());
    }

    #[test]
    fn stripe_req_bypass_is_not_serialized() {
        // `bypass` is `#[serde(skip)]`: it never crosses the fabric, so
        // a request decoded from the wire is always non-bypass even if
        // the local request carried the flag.
        let origin = OriginRef::new("primary-s3", "models/llama.bin", 7);
        let req = StripeReq::new(origin.stripe_key())
            .with_origin(origin)
            .with_bypass(true);
        let bytes = bincode::serialize(&req).unwrap();
        let back: StripeReq = bincode::deserialize(&bytes).unwrap();
        assert!(!back.bypass());
    }

    #[test]
    fn stripe_req_skip_local_disk_is_not_serialized() {
        // `skip_local_disk` is local to the requester: peer handlers must
        // still read from their own NVMe cache after decoding the request.
        let origin = OriginRef::new("primary-s3", "models/llama.bin", 7);
        let req = StripeReq::new(origin.stripe_key())
            .with_origin(origin)
            .with_skip_local_disk(true);
        let bytes = bincode::serialize(&req).unwrap();
        let back: StripeReq = bincode::deserialize(&bytes).unwrap();
        assert!(!back.skip_local_disk());
    }

    #[test]
    fn stripe_req_cache_context_round_trips_through_bincode() {
        let origin = OriginRef::new("primary-s3", "models/llama.bin", 7);
        let req = StripeReq::new(origin.stripe_key())
            .with_origin(origin.clone())
            .with_cache_id(Some("cache-a".to_string()))
            .with_bypass(true);

        let bytes = bincode::serialize(&req).unwrap();
        let back: StripeReq = bincode::deserialize(&bytes).unwrap();

        assert_eq!(back.origin(), Some(&origin));
        assert_eq!(back.cache_id(), Some("cache-a"));
        assert!(!back.bypass());
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

    #[test]
    fn metadata_request_rejects_expired_cached_page() {
        let origin = OriginRef::metadata_entry("primary", "/object");
        let req = StripeReq::new(origin.stripe_key()).with_origin(origin);
        let page = ObjectMetadata::found(12, 100).encode().unwrap();

        assert!(req.cached_page_valid_at(0, &page, 99));
        assert!(!req.cached_page_valid_at(0, &page, 100));
        assert!(!req.cached_page_valid_at(0, &page, 101));
    }

    #[test]
    fn metadata_request_accepts_unexpired_negative_page() {
        let origin = OriginRef::metadata_entry("primary", "/missing");
        let req = StripeReq::new(origin.stripe_key()).with_origin(origin);
        let page = ObjectMetadata::not_found(10).encode().unwrap();

        assert!(req.cached_page_valid_at(0, &page, 9));
        assert!(!req.cached_page_valid_at(0, &page, 10));
    }

    #[test]
    fn metadata_request_rejects_malformed_cached_page() {
        let origin = OriginRef::metadata_entry("primary", "/broken");
        let req = StripeReq::new(origin.stripe_key()).with_origin(origin);

        assert!(!req.cached_page_valid_at(0, &[0u8; 4], 0));
    }

    #[test]
    fn cache_validity_hook_only_applies_to_metadata_entry_head_page() {
        let data_origin = OriginRef::new("primary", "/object", 0);
        let data_req = StripeReq::new(data_origin.stripe_key()).with_origin(data_origin);
        let metadata_origin = OriginRef::metadata_entry("primary", "/object");
        let metadata_req =
            StripeReq::new(metadata_origin.stripe_key()).with_origin(metadata_origin);
        let expired = ObjectMetadata::found(12, 100).encode().unwrap();

        assert!(data_req.cached_page_valid_at(0, &expired, 100));
        assert!(metadata_req.cached_page_valid_at(4096, &expired, 100));
    }
}
