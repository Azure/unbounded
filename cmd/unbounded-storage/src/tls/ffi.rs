// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Hand-written OpenSSL FFI declarations plus shim externs for the
//! macro-only entry points compiled in `src/tls/shim.c`.
//!
//! The surface is deliberately minimal: enough to drive a client TLS
//! handshake over a caller-owned socket fd and to enable kernel TLS so
//! the post-handshake data path stays in the kernel (zero-copy
//! `recv`/`send` straight to/from the registered backing). Rust never
//! reads OpenSSL struct fields; every object is an opaque pointer.

#![allow(non_camel_case_types, non_upper_case_globals)]

use std::os::raw::{c_char, c_int, c_long, c_ulong, c_void};

// Opaque OpenSSL handles. Rust only ever holds pointers to these.
pub enum SSL_CTX {}
pub enum SSL {}
pub enum SSL_METHOD {}

// SSL_get_error return codes (openssl/ssl.h).
pub const SSL_ERROR_SSL: c_int = 1;
pub const SSL_ERROR_WANT_READ: c_int = 2;
pub const SSL_ERROR_WANT_WRITE: c_int = 3;
pub const SSL_ERROR_SYSCALL: c_int = 5;
pub const SSL_ERROR_ZERO_RETURN: c_int = 6;

// SSL_CTX_set_verify modes (openssl/ssl.h).
pub const SSL_VERIFY_NONE: c_int = 0x00;
pub const SSL_VERIFY_PEER: c_int = 0x01;

// X509_V_OK: SSL_get_verify_result success value (openssl/x509_vfy.h).
pub const X509_V_OK: c_long = 0;

unsafe extern "C" {
    // Exported OpenSSL symbols (real functions, not macros).
    pub fn TLS_client_method() -> *const SSL_METHOD;
    pub fn SSL_CTX_new(method: *const SSL_METHOD) -> *mut SSL_CTX;
    pub fn SSL_CTX_free(ctx: *mut SSL_CTX);
    pub fn SSL_CTX_set_default_verify_paths(ctx: *mut SSL_CTX) -> c_int;
    pub fn SSL_CTX_load_verify_locations(
        ctx: *mut SSL_CTX,
        ca_file: *const c_char,
        ca_path: *const c_char,
    ) -> c_int;
    pub fn SSL_CTX_set_verify(
        ctx: *mut SSL_CTX,
        mode: c_int,
        callback: Option<extern "C" fn(c_int, *mut c_void) -> c_int>,
    );

    pub fn SSL_new(ctx: *mut SSL_CTX) -> *mut SSL;
    pub fn SSL_free(ssl: *mut SSL);
    pub fn SSL_set_fd(ssl: *mut SSL, fd: c_int) -> c_int;
    pub fn SSL_set1_host(ssl: *mut SSL, hostname: *const c_char) -> c_int;
    pub fn SSL_set_connect_state(ssl: *mut SSL);
    pub fn SSL_connect(ssl: *mut SSL) -> c_int;
    pub fn SSL_get_error(ssl: *const SSL, ret: c_int) -> c_int;
    pub fn SSL_get_verify_result(ssl: *const SSL) -> c_long;

    pub fn ERR_get_error() -> c_ulong;
    pub fn ERR_error_string_n(e: c_ulong, buf: *mut c_char, len: usize);
    pub fn ERR_clear_error();

    // Macro-only entry points exported via src/tls/shim.c.
    pub fn ub_ssl_ctx_set_options(ctx: *mut SSL_CTX, op: c_ulong) -> c_ulong;
    pub fn ub_ssl_ctx_set_min_proto_version(ctx: *mut SSL_CTX, version: c_int) -> c_int;
    pub fn ub_ssl_set_tlsext_host_name(ssl: *mut SSL, name: *const c_char) -> c_long;
    pub fn ub_ssl_ktls_send_enabled(ssl: *mut SSL) -> c_int;
    pub fn ub_ssl_ktls_recv_enabled(ssl: *mut SSL) -> c_int;
    pub fn ub_ssl_op_enable_ktls() -> c_ulong;
    pub fn ub_tls1_2_version() -> c_int;
}
