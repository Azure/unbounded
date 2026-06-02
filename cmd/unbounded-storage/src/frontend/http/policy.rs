// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! The HTTP [`ServePolicy`]: serve cached objects keyed by raw request
//! path, resolving object length from an origin `HEAD` (cached
//! per-shard) and streaming the body across the object's stripes.

use std::cell::RefCell;
use std::time::Instant;

use ::http::header::{ACCEPT_RANGES, CONNECTION, CONTENT_LENGTH, CONTENT_RANGE, HOST};
use ::http::{Request, Response, StatusCode};

use crate::frontend::cache::{Tick, TtlCache};
use crate::frontend::conn::{
    Action, BodyStripe, FdGuard, MAX_HEADER_BYTES, RECV_CHUNK, ServePolicy, open_tcp_v4,
    split_query,
};
use crate::frontend::range::{ByteRange, RangeError, ResolvedRange, full_object, stripe_set};
use crate::http::{HttpRequest, Method, ResponseHead, serialize_request, serialize_response_head};
use crate::ring::{NetHandle, SockAddr};
use crate::storage::{OriginRef, StripeReq};

/// Per-shard HTTP serving policy. Holds the immutable origin coordinates
/// plus the object-length cache, all shard-local. Shared across the
/// shard's connections behind an `Rc`; `respond` takes `&self` and the
/// cache is a `RefCell` (single-threaded shard, no contention).
pub struct HttpPolicy {
    backend_id: String,
    stripe_size: u64,
    origin: SockAddr,
    origin_host: String,
    len_cache: RefCell<TtlCache<String, u64>>,
    epoch: Instant,
}

impl HttpPolicy {
    /// Build a policy. `origin` is the resolved origin address used for
    /// `HEAD` object-length lookups; the `Host:` header is rendered from
    /// it. `stripe_size` comes from the shard's pool geometry;
    /// `meta_ttl_ms` is the object-length cache TTL.
    pub fn new(backend_id: String, stripe_size: u64, origin: SockAddr, meta_ttl_ms: u64) -> Self {
        let origin_host = origin
            .as_ipv4()
            .map(|(ip, port)| format!("{ip}:{port}"))
            .unwrap_or_else(|| "origin".to_string());
        Self {
            backend_id,
            stripe_size,
            origin,
            origin_host,
            len_cache: RefCell::new(TtlCache::new(meta_ttl_ms)),
            epoch: Instant::now(),
        }
    }

    /// Resolve an object's length: cached, else an origin `HEAD`.
    async fn resolve_len(&self, handle: &NetHandle, path: &str) -> Option<u64> {
        let now = now_ms(self.epoch);
        if let Some(l) = self
            .len_cache
            .borrow_mut()
            .get(now, &path.to_string())
            .copied()
        {
            return Some(l);
        }
        let l = origin_head_length(handle, &self.origin, &self.origin_host, path)
            .await
            .ok()?;
        self.len_cache.borrow_mut().insert(now, path.to_string(), l);
        Some(l)
    }
}

impl ServePolicy for HttpPolicy {
    async fn respond(&self, handle: &NetHandle, req: &HttpRequest<'_>) -> Action {
        // Only GET and HEAD are served. HEAD resolves length and builds
        // the same head as GET but never streams a body.
        let is_head = match req.method {
            Method::GET => false,
            Method::HEAD => true,
            _ => return Action::Respond(status_line_response(405)),
        };

        let path = split_query(req.target).0.to_string();

        // Optional Range header.
        let range = match req.header("range") {
            Some(v) => match ByteRange::parse(v) {
                Ok(r) => Some(r),
                Err(_) => return Action::Respond(status_line_response(400)),
            },
            None => None,
        };

        // Resolve object length (cached, else origin HEAD). A failure
        // here means we cannot answer; drop the connection rather than
        // invent a length.
        let len = match self.resolve_len(handle, &path).await {
            Some(l) => l,
            None => return Action::Close,
        };

        // Resolve the requested range against the length.
        let resolved = match range {
            None => full_object(len),
            Some(br) => match br.resolve(len) {
                Ok(r) => r,
                Err(RangeError::Unsatisfiable { object_len }) => {
                    return Action::Respond(unsatisfiable_response(object_len));
                }
                Err(_) => return Action::Respond(status_line_response(400)),
            },
        };

        let head = if range.is_some() {
            partial_head(resolved, len)
        } else {
            full_head(len)
        };

        if is_head {
            return Action::Respond(head);
        }

        // Build the body plan: one stripe slice per object stripe the
        // resolved range covers, each carrying its origin mapping so an
        // origin miss can be resolved by the backend tier.
        let body = stripe_set(resolved, self.stripe_size)
            .into_iter()
            .map(|slice| {
                let origin_ref = OriginRef {
                    backend_id: self.backend_id.clone(),
                    origin_object_id: path.clone(),
                    stripe_idx: slice.stripe_idx,
                };
                let req = StripeReq::new(origin_ref.stripe_key()).with_origin(origin_ref);
                BodyStripe {
                    req,
                    intra_offset: slice.intra_offset,
                    intra_len: slice.intra_len,
                }
            })
            .collect();

        Action::Stream { head, body }
    }

    fn malformed_request(&self) -> Vec<u8> {
        status_line_response(400)
    }
}

/// Resolve an object's length by issuing a `HEAD` to the origin and
/// reading back its `Content-Length`. One connection per lookup
/// (`Connection: close`), matching the cold-path origin backend.
async fn origin_head_length(
    handle: &NetHandle,
    origin: &SockAddr,
    host: &str,
    path: &str,
) -> std::io::Result<u64> {
    let conn = open_tcp_v4()?;
    let _g = FdGuard(conn);
    handle.connect(conn, origin.duplicate()).await?;
    let request = Request::builder()
        .method(Method::HEAD)
        .uri(path)
        .header(HOST, host)
        .header(CONNECTION, "close")
        .body(())
        .map_err(|_| {
            std::io::Error::new(
                std::io::ErrorKind::InvalidData,
                "failed to build origin HEAD request",
            )
        })?;
    handle.send(conn, serialize_request(&request)).await?;

    let mut buf: Vec<u8> = Vec::new();
    loop {
        match ResponseHead::parse(&buf) {
            Ok(Some(head)) => {
                if head.status != StatusCode::OK {
                    return Err(std::io::Error::new(
                        std::io::ErrorKind::InvalidData,
                        "origin HEAD response was not 200 OK",
                    ));
                }
                return head.content_length().ok_or_else(|| {
                    std::io::Error::new(
                        std::io::ErrorKind::InvalidData,
                        "origin HEAD response missing Content-Length",
                    )
                });
            }
            Ok(None) => {}
            Err(_) => {
                return Err(std::io::Error::new(
                    std::io::ErrorKind::InvalidData,
                    "origin HEAD response head malformed",
                ));
            }
        }
        if buf.len() > MAX_HEADER_BYTES {
            return Err(std::io::Error::new(
                std::io::ErrorKind::InvalidData,
                "origin HEAD response head too large",
            ));
        }
        let chunk = handle.recv(conn, RECV_CHUNK).await?;
        if chunk.is_empty() {
            return Err(std::io::Error::new(
                std::io::ErrorKind::UnexpectedEof,
                "origin closed before HEAD response complete",
            ));
        }
        buf.extend_from_slice(&chunk);
    }
}

/// Format the `200 OK` head for serving a whole object.
fn full_head(len: u64) -> Vec<u8> {
    let resp = Response::builder()
        .status(StatusCode::OK)
        .header(CONTENT_LENGTH, len.to_string())
        .header(ACCEPT_RANGES, "bytes")
        .header(CONNECTION, "close")
        .body(())
        .expect("valid full-object response head");
    serialize_response_head(&resp)
}

/// Format the `206 Partial Content` head for a resolved byte range.
/// `END` in `Content-Range` is inclusive (`resolved.end - 1`).
fn partial_head(resolved: ResolvedRange, total: u64) -> Vec<u8> {
    let start = resolved.start;
    let end_incl = resolved.end - 1;
    let clen = resolved.len();
    let resp = Response::builder()
        .status(StatusCode::PARTIAL_CONTENT)
        .header(CONTENT_RANGE, format!("bytes {start}-{end_incl}/{total}"))
        .header(CONTENT_LENGTH, clen.to_string())
        .header(ACCEPT_RANGES, "bytes")
        .header(CONNECTION, "close")
        .body(())
        .expect("valid partial-content response head");
    serialize_response_head(&resp)
}

/// Format a `416 Range Not Satisfiable` head with `Content-Range: bytes
/// */LEN`.
fn unsatisfiable_response(total: u64) -> Vec<u8> {
    let resp = Response::builder()
        .status(StatusCode::RANGE_NOT_SATISFIABLE)
        .header(CONTENT_RANGE, format!("bytes */{total}"))
        .header(CONTENT_LENGTH, "0")
        .header(CONNECTION, "close")
        .body(())
        .expect("valid unsatisfiable-range response head");
    serialize_response_head(&resp)
}

/// Format a bodyless status-line response (`Content-Length: 0`) for the
/// simple error statuses this frontend emits.
fn status_line_response(status: u16) -> Vec<u8> {
    let status = StatusCode::from_u16(status).unwrap_or(StatusCode::INTERNAL_SERVER_ERROR);
    let resp = Response::builder()
        .status(status)
        .header(CONTENT_LENGTH, "0")
        .header(CONNECTION, "close")
        .body(())
        .expect("valid status-line response head");
    serialize_response_head(&resp)
}

/// Milliseconds since `epoch`, the tick unit for the object-length
/// cache.
fn now_ms(epoch: Instant) -> Tick {
    epoch.elapsed().as_millis() as Tick
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::frontend::range::StripeSlice;

    #[test]
    fn full_head_exact_bytes() {
        let head = full_head(4096);
        let s = std::str::from_utf8(&head).unwrap();
        assert!(s.starts_with("HTTP/1.1 200 OK\r\n"), "got: {s}");
        assert!(s.contains("content-length: 4096\r\n"), "got: {s}");
        assert!(s.contains("accept-ranges: bytes\r\n"), "got: {s}");
        assert!(s.contains("connection: close\r\n"), "got: {s}");
        assert!(s.ends_with("\r\n\r\n"), "got: {s}");
    }

    #[test]
    fn partial_head_exact_bytes_inclusive_end() {
        let head = partial_head(ResolvedRange { start: 0, end: 100 }, 1000);
        let s = std::str::from_utf8(&head).unwrap();
        assert!(
            s.starts_with("HTTP/1.1 206 Partial Content\r\n"),
            "got: {s}"
        );
        assert!(s.contains("content-range: bytes 0-99/1000\r\n"), "got: {s}");
        assert!(s.contains("content-length: 100\r\n"), "got: {s}");
        assert!(s.contains("accept-ranges: bytes\r\n"), "got: {s}");
        assert!(s.contains("connection: close\r\n"), "got: {s}");
        assert!(s.ends_with("\r\n\r\n"), "got: {s}");
    }

    #[test]
    fn partial_head_mid_object() {
        let head = partial_head(
            ResolvedRange {
                start: 70,
                end: 100,
            },
            100,
        );
        let s = std::str::from_utf8(&head).unwrap();
        assert!(s.contains("content-range: bytes 70-99/100\r\n"), "got: {s}");
        assert!(s.contains("content-length: 30\r\n"), "got: {s}");
    }

    #[test]
    fn unsatisfiable_response_exact_bytes() {
        let head = unsatisfiable_response(100);
        let s = std::str::from_utf8(&head).unwrap();
        assert!(
            s.starts_with("HTTP/1.1 416 Range Not Satisfiable\r\n"),
            "got: {s}"
        );
        assert!(s.contains("content-range: bytes */100\r\n"), "got: {s}");
        assert!(s.contains("content-length: 0\r\n"), "got: {s}");
        assert!(s.contains("connection: close\r\n"), "got: {s}");
        assert!(s.ends_with("\r\n\r\n"), "got: {s}");
    }

    #[test]
    fn status_405_and_400_exact_bytes() {
        let r405 = status_line_response(405);
        let s405 = std::str::from_utf8(&r405).unwrap();
        assert!(
            s405.starts_with("HTTP/1.1 405 Method Not Allowed\r\n"),
            "got: {s405}"
        );
        assert!(s405.contains("content-length: 0\r\n"), "got: {s405}");
        assert!(s405.contains("connection: close\r\n"), "got: {s405}");
        assert!(s405.ends_with("\r\n\r\n"), "got: {s405}");

        let r400 = status_line_response(400);
        let s400 = std::str::from_utf8(&r400).unwrap();
        assert!(
            s400.starts_with("HTTP/1.1 400 Bad Request\r\n"),
            "got: {s400}"
        );
        assert!(s400.contains("content-length: 0\r\n"), "got: {s400}");
        assert!(s400.contains("connection: close\r\n"), "got: {s400}");
        assert!(s400.ends_with("\r\n\r\n"), "got: {s400}");
    }

    #[test]
    fn range_to_stripe_set_wiring_no_range() {
        let resolved = full_object(10);
        let slices = stripe_set(resolved, 4);
        assert_eq!(
            slices,
            vec![
                StripeSlice {
                    stripe_idx: 0,
                    intra_offset: 0,
                    intra_len: 4
                },
                StripeSlice {
                    stripe_idx: 1,
                    intra_offset: 0,
                    intra_len: 4
                },
                StripeSlice {
                    stripe_idx: 2,
                    intra_offset: 0,
                    intra_len: 2
                },
            ]
        );
    }

    #[test]
    fn range_to_stripe_set_wiring_with_range() {
        let br = ByteRange::parse("bytes=5-6").unwrap();
        let resolved = br.resolve(10).unwrap();
        assert_eq!(resolved, ResolvedRange { start: 5, end: 7 });
        let slices = stripe_set(resolved, 4);
        assert_eq!(
            slices,
            vec![StripeSlice {
                stripe_idx: 1,
                intra_offset: 1,
                intra_len: 2
            }]
        );
    }

    #[test]
    fn unsatisfiable_range_resolves_to_error() {
        let br = ByteRange::parse("bytes=100-200").unwrap();
        assert!(matches!(
            br.resolve(100),
            Err(RangeError::Unsatisfiable { object_len: 100 })
        ));
    }
}
