// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! TLS for origin and peer connections, built on OpenSSL 3 with kernel
//! TLS (kTLS).
//!
//! This module is the crate's shared, transport-level TLS utility, a
//! sibling to [`crate::http`]: it owns the OpenSSL FFI surface, the
//! client `SSL_CTX`/handshake driver, and the record-aware receive
//! helpers that keep the post-handshake data path zero-copy. The origin
//! backends (`http`, `s3`, `azure`) build a [`TlsContext`] from their
//! config, drive [`TlsContext::handshake`] over a caller-owned socket
//! fd, and then receive bodies through [`recv_chunk`]/[`recv_fixed`].
//!
//! Everything here is Linux- and OpenSSL-specific: the handshake enables
//! kernel TLS so the kernel performs the symmetric crypto and io_uring
//! `recv`/`send` carry plaintext straight to/from the registered
//! backing. See [`context`] for the OpenSSL >= 3.5 requirement and the
//! kTLS engagement assertions.

mod context;
mod ffi;
mod peer;
mod recv;

pub use context::{TlsConfig, TlsContext};
pub use peer::{PeerCertificateIdentity, PeerTlsConfig, PeerTlsContext};

/// Drive the optional client handshake for an origin connection.
pub(crate) use context::maybe_handshake;

/// Record-aware receive helpers used by the origin backends to read
/// response bytes off a kTLS (or plaintext) connection.
pub(crate) use recv::{recv_chunk, recv_fixed};
