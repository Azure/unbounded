// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! In-memory implementation of `ObjectSource` for testing.
//! NOT compiled into production binaries.

use std::collections::HashMap;

use bytes::Bytes;
use futures::stream::{self, BoxStream};
use futures::StreamExt;
use unbounded_storage::bufferpool::StripeKey;

use super::error::Error;
use super::traits::ObjectSource;
use crate::catalog::ObjectMeta;

/// Test-only source fed from a `HashMap<StripeKey, Bytes>`.
pub struct MemoryObjectSource {
    data: HashMap<StripeKey, Bytes>,
}

impl MemoryObjectSource {
    pub fn new(data: HashMap<StripeKey, Bytes>) -> Self {
        Self { data }
    }

    pub fn single(stripe: StripeKey, content: Bytes) -> Self {
        let mut m = HashMap::new();
        m.insert(stripe, content);
        Self { data: m }
    }

    pub fn insert(&mut self, meta: &ObjectMeta, content: Bytes) {
        self.data.insert(meta.stripe, content);
    }
}

impl ObjectSource for MemoryObjectSource {
    fn read_range(
        &self,
        meta: &ObjectMeta,
        offset: u64,
        len: u64,
    ) -> BoxStream<'static, Result<Bytes, Error>> {
        let buf = match self.data.get(&meta.stripe) {
            Some(b) => b,
            None => {
                let stripe_byte = meta.stripe.0[0];
                return internal_err_stream(format!(
                    "stripe {stripe_byte:02x}.. not found in memory source",
                ));
            }
        };

        let (off, end) = match resolve_indices(offset, len) {
            Ok(None) => return stream::empty().boxed(),
            Ok(Some(r)) => r,
            Err(msg) => return internal_err_stream(format!("memory source: {msg}")),
        };
        if end > buf.len() {
            // Fail loudly: a catalog/source length mismatch in a test
            // shouldn't silently produce a body shorter than the
            // advertised `Content-Length`.
            let buf_len = buf.len();
            return internal_err_stream(format!(
                "memory source range {off}..{end} exceeds buffer len {buf_len}"
            ));
        }
        // `ObjectSource` requires each yielded chunk to be an
        // independent heap allocation, not a shared view into the
        // source buffer. The production impl honors this by copying
        // out of a shared ring; we do the equivalent here so tests
        // exercise the same trait contract.
        let chunk = Bytes::copy_from_slice(&buf[off..end]);
        stream::once(async move { Ok(chunk) }).boxed()
    }
}

/// Resolve `(offset, len)` against a target buffer, producing the
/// half-open `[off, end)` byte range to slice or an error.
///
/// Returns `Ok(None)` when `len == 0` so the caller can short-circuit
/// without slicing. Returns `Err` when any of:
/// - `offset` doesn't fit in `usize` (32-bit hosts only),
/// - `len` doesn't fit in `usize` (32-bit hosts only),
/// - `off + len_usize` overflows `usize` (reachable on any host with
///   adversarial inputs near `usize::MAX`).
///
/// Pulled out so each arm is unit-testable without depending on the
/// host's pointer width. The two `try_from` arms cannot fail on
/// 64-bit targets (the CI configuration); the `checked_add` arm is
/// always exercisable.
fn resolve_indices(offset: u64, len: u64) -> Result<Option<(usize, usize)>, String> {
    if len == 0 {
        return Ok(None);
    }
    let off = usize::try_from(offset)
        .map_err(|_| format!("offset {offset} exceeds usize::MAX"))?;
    let len_usize = usize::try_from(len)
        .map_err(|_| format!("len {len} exceeds usize::MAX"))?;
    let end = off
        .checked_add(len_usize)
        .ok_or_else(|| format!("offset {offset} + len {len} overflows usize"))?;
    Ok(Some((off, end)))
}

/// Helper that wraps a single `Internal` error into a one-item
/// `BoxStream`. Used by every error arm in `read_range` so the
/// boilerplate stays in one place.
fn internal_err_stream(msg: String) -> BoxStream<'static, Result<Bytes, Error>> {
    stream::once(async move { Err(Error::Internal(msg)) }).boxed()
}

#[cfg(test)]
mod tests {
    use super::*;
    use futures::StreamExt;

    fn meta(stripe: StripeKey, size: u64) -> ObjectMeta {
        ObjectMeta {
            stripe,
            size,
            etag: "\"deadbeefdeadbeef\"".into(),
            content_type: "application/octet-stream".into(),
            last_modified: "Thu, 01 Jan 1970 00:00:00 GMT".into(),
        }
    }

    #[tokio::test]
    async fn out_of_range_yields_internal_error() {
        let stripe = StripeKey([0xab; 32]);
        let src = MemoryObjectSource::single(stripe, Bytes::from_static(b"hello"));
        let mut s = src.read_range(&meta(stripe, 5), 0, 10);
        let first = s.next().await.expect("stream yields one item");
        match first {
            Err(Error::Internal(msg)) => {
                assert!(
                    msg.contains("exceeds buffer len 5"),
                    "unexpected message: {msg}",
                );
            }
            other => panic!("expected Internal error, got {other:?}"),
        }
        assert!(s.next().await.is_none(), "stream should end after error");
    }

    #[tokio::test]
    async fn missing_stripe_yields_internal_error() {
        let src = MemoryObjectSource::new(HashMap::new());
        let stripe = StripeKey([0u8; 32]);
        let mut s = src.read_range(&meta(stripe, 1), 0, 1);
        let first = s.next().await.expect("stream yields one item");
        assert!(matches!(first, Err(Error::Internal(_))));
    }

    #[tokio::test]
    async fn offset_plus_len_overflow_yields_internal_error() {
        // A `(offset, len)` pair where `offset + len` wraps `usize`
        // must produce a clean `Internal` error rather than panicking
        // in the slice operation.
        let stripe = StripeKey([0xab; 32]);
        let src = MemoryObjectSource::single(stripe, Bytes::from_static(b"hello"));
        let mut s = src.read_range(&meta(stripe, 5), u64::MAX, 1);
        let first = s.next().await.expect("stream yields one item");
        match first {
            Err(Error::Internal(msg)) => {
                assert!(msg.contains("overflows usize"), "unexpected message: {msg}");
            }
            other => panic!("expected Internal error, got {other:?}"),
        }
    }

    // ---- resolve_indices unit tests ---------------------------------------
    //
    // These run synchronously and exercise the pure function directly
    // so the validation branches don't depend on the host's pointer
    // width or on the surrounding `read_range` plumbing.

    #[test]
    fn resolve_indices_zero_length_short_circuits() {
        // Zero length always returns `None`, regardless of offset, so
        // the caller can short-circuit before slicing.
        assert_eq!(resolve_indices(0, 0).unwrap(), None);
        assert_eq!(resolve_indices(u64::MAX, 0).unwrap(), None);
    }

    #[test]
    fn resolve_indices_basic_range() {
        assert_eq!(resolve_indices(5, 10).unwrap(), Some((5, 15)));
        assert_eq!(resolve_indices(0, 1).unwrap(), Some((0, 1)));
    }

    #[test]
    fn resolve_indices_checked_add_overflow() {
        // `usize::MAX + 1` (as u64 inputs) must produce an `Err` from
        // the `checked_add` arm. Deterministic on both 32-bit and
        // 64-bit because we anchor on `usize::MAX`.
        let big = usize::MAX as u64;
        let err = resolve_indices(big, 1).unwrap_err();
        assert!(err.contains("overflows usize"), "unexpected message: {err}");
    }

    #[test]
    #[cfg(target_pointer_width = "32")]
    fn resolve_indices_offset_try_from_failure() {
        // Only reachable on 32-bit hosts; documents the contract that
        // a u64 offset exceeding `usize::MAX` produces a clean error.
        let err = resolve_indices(0x1_0000_0000, 1).unwrap_err();
        assert!(
            err.contains("offset 4294967296 exceeds usize::MAX"),
            "unexpected message: {err}",
        );
    }

    #[test]
    #[cfg(target_pointer_width = "32")]
    fn resolve_indices_len_try_from_failure() {
        // Only reachable on 32-bit hosts; documents that a u64 len
        // exceeding `usize::MAX` produces a clean error.
        let err = resolve_indices(0, 0x1_0000_0000).unwrap_err();
        assert!(
            err.contains("len 4294967296 exceeds usize::MAX"),
            "unexpected message: {err}",
        );
    }
}
