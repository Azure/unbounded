// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Transport-agnostic HTTP/1.1 wire codec shared by the storage
//! frontends and backends.
//!
//! This module owns the low-level, opinion-free HTTP/1.1 parsing and
//! serialization the crate needs: request heads (server-side parse,
//! client-side serialize) and response heads (client-side parse,
//! server-side serialize). Parsing is delegated to [`httparse`]; the
//! typed [`Method`]/[`StatusCode`]/header building blocks come from the
//! [`http`] crate. The leading `::` on `::http` paths inside this module
//! names that external crate, distinguishing it from this `crate::http`
//! module.
//!
//! Everything here is deliberately free of any storage-specific policy
//! (ranges, stripe geometry, which status a frontend returns) so it can
//! back both the current plaintext HTTP frontend/backend and a future
//! S3-compatible pair. The opinionated transport semantics live with
//! their respective frontend/backend implementations and call into this
//! module for the wire encoding/decoding.
//!
//! The parsers borrow from a caller-owned byte buffer and return
//! borrowed header views (zero-copy on the hot path); the serializers
//! build an owned `Vec<u8>` from an [`http::Request`]/[`http::Response`].

mod headers;
mod request;
mod response;

pub use headers::{Header, ParseError};
pub use request::{HttpRequest, serialize_request};
pub use response::{ResponseHead, serialize_response_head};

/// Canonical typed `Method`/`StatusCode`, re-exported from the [`http`]
/// crate so the rest of the crate has a single source for them.
pub use ::http::{Method, StatusCode};
