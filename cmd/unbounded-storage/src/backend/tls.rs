// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Client-side TLS for the origin backends, built on OpenSSL 3 with
//! kernel TLS (kTLS).
//!
//! The handshake runs in userspace over the caller-owned socket fd, but
//! we set `SSL_OP_ENABLE_KTLS` so OpenSSL hands the negotiated keys to
//! the kernel (`setsockopt(SOL_TLS, ...)`). After a successful handshake
//! the kernel performs the symmetric crypto, so the backend keeps using
//! plain io_uring `send`/`recv_fixed_msg` against the same fd: the body
//! lands decrypted directly in the registered backing (zero copy
//! preserved). We assert both kTLS directions actually engaged and fail
//! loudly otherwise rather than silently falling back to a copying path.
//!
//! kTLS receive on TLS 1.3 requires OpenSSL >= 3.5: OpenSSL 3.0.x only
//! wires up kTLS for the send direction on 1.3 (`tls13_change_cipher_state`
//! skips the read side), so the receive-offload assertion would fail
//! there. The build links a pinned OpenSSL >= 3.5 bundled under
//! `tmp/openssl/<version>/` (see the Makefile `openssl` target), not the
//! system library, precisely so TLS 1.3 keeps full zero-copy receive.
//!
//! The handshake never blocks the shard: `SSL_connect` runs against a
//! non-blocking fd and `WANT_READ`/`WANT_WRITE` are driven through the
//! ring's `poll_ready` (io_uring `POLL_ADD`).

use std::ffi::CString;
use std::fmt;
use std::os::fd::RawFd;

use super::tls_ffi;
use crate::bufferpool::Error;
use crate::ring::NetHandle;

/// Per-backend TLS knobs derived from the backend config.
#[derive(Debug, Clone, Default)]
pub struct TlsConfig {
    /// Optional path to a PEM CA bundle to trust in addition to the
    /// system trust store. Mutually exclusive with `insecure_skip_verify`
    /// (rejected at config validation time).
    pub ca_cert_path: Option<String>,
    /// When true, skip certificate and hostname verification entirely.
    /// For test/private origins only.
    pub insecure_skip_verify: bool,
}

/// A configured client `SSL_CTX`, shared by every connection a backend
/// makes. Shard-pinned: a backend and its `TlsContext` live on one
/// thread, so the raw `SSL_CTX` pointer is never touched concurrently.
pub struct TlsContext {
    ctx: *mut tls_ffi::SSL_CTX,
    verify: bool,
}

// SAFETY: the same shard-pinned justification as the backends. The
// `SSL_CTX` is only used from the single thread that owns the backend;
// the Send + Sync bounds exist solely so the backend can satisfy the
// `Backend: Send + Sync` supertrait.
unsafe impl Send for TlsContext {}
unsafe impl Sync for TlsContext {}

impl TlsContext {
    /// Build a client TLS context from `config`.
    pub fn new(config: &TlsConfig) -> Result<Self, Error> {
        // SAFETY: OpenSSL 3 self-initializes on first use; every call
        // below is a standard libssl entry point with checked returns.
        unsafe {
            let method = tls_ffi::TLS_client_method();
            if method.is_null() {
                return Err(ssl_error("TLS_client_method returned null"));
            }

            let ctx = tls_ffi::SSL_CTX_new(method);
            if ctx.is_null() {
                return Err(ssl_error("SSL_CTX_new returned null"));
            }

            // From here on, free the ctx on any error.
            let guard = CtxGuard { ctx };

            if tls_ffi::ub_ssl_ctx_set_min_proto_version(ctx, tls_ffi::ub_tls1_2_version()) != 1 {
                return Err(ssl_error("SSL_CTX_set_min_proto_version failed"));
            }

            // Ask OpenSSL to enable kernel TLS for connections from this
            // context. The actual engagement is asserted post-handshake.
            tls_ffi::ub_ssl_ctx_set_options(ctx, tls_ffi::ub_ssl_op_enable_ktls());

            let verify = !config.insecure_skip_verify;
            if verify {
                tls_ffi::SSL_CTX_set_verify(ctx, tls_ffi::SSL_VERIFY_PEER, None);
                if tls_ffi::SSL_CTX_set_default_verify_paths(ctx) != 1 {
                    return Err(ssl_error("SSL_CTX_set_default_verify_paths failed"));
                }
                if let Some(path) = config.ca_cert_path.as_deref() {
                    let c_path = CString::new(path)
                        .map_err(|_| Error::transport(TlsError("ca_cert_path contains NUL".into())))?;
                    if tls_ffi::SSL_CTX_load_verify_locations(
                        ctx,
                        c_path.as_ptr(),
                        std::ptr::null(),
                    ) != 1
                    {
                        return Err(ssl_error("SSL_CTX_load_verify_locations failed"));
                    }
                }
            } else {
                tls_ffi::SSL_CTX_set_verify(ctx, tls_ffi::SSL_VERIFY_NONE, None);
            }

            guard.disarm();
            Ok(TlsContext { ctx, verify })
        }
    }

    /// Drive a client TLS handshake to completion over `fd`, validating
    /// `host` (SNI + certificate hostname). On success the kernel TLS
    /// data path is engaged for both directions and `fd` carries
    /// plaintext to/from io_uring.
    pub async fn handshake(&self, handle: &NetHandle, fd: RawFd, host: &str) -> Result<(), Error> {
        let c_host = CString::new(host)
            .map_err(|_| Error::transport(TlsError("host contains NUL".into())))?;

        // SAFETY: `ssl` is freed by `SslGuard` on every path. `fd` is
        // owned by the caller for the duration of the handshake.
        let ssl = unsafe { tls_ffi::SSL_new(self.ctx) };
        if ssl.is_null() {
            return Err(ssl_error("SSL_new returned null"));
        }
        let ssl = SslGuard { ssl };

        // SAFETY: standard libssl client setup on a fresh SSL object.
        unsafe {
            if tls_ffi::SSL_set_fd(ssl.ssl, fd) != 1 {
                return Err(ssl_error("SSL_set_fd failed"));
            }
            if tls_ffi::ub_ssl_set_tlsext_host_name(ssl.ssl, c_host.as_ptr()) != 1 {
                return Err(ssl_error("SSL_set_tlsext_host_name failed"));
            }
            if self.verify && tls_ffi::SSL_set1_host(ssl.ssl, c_host.as_ptr()) != 1 {
                return Err(ssl_error("SSL_set1_host failed"));
            }
            tls_ffi::SSL_set_connect_state(ssl.ssl);
        }

        set_nonblocking(fd)?;

        loop {
            // SAFETY: ssl is valid; SSL_connect drives the handshake
            // against the non-blocking fd.
            let ret = unsafe { tls_ffi::SSL_connect(ssl.ssl) };
            if ret == 1 {
                break;
            }
            let err = unsafe { tls_ffi::SSL_get_error(ssl.ssl, ret) };
            match err {
                tls_ffi::SSL_ERROR_WANT_READ => {
                    handle
                        .poll_ready(fd, libc::POLLIN as u32)
                        .await
                        .map_err(Error::transport)?;
                }
                tls_ffi::SSL_ERROR_WANT_WRITE => {
                    handle
                        .poll_ready(fd, libc::POLLOUT as u32)
                        .await
                        .map_err(Error::transport)?;
                }
                tls_ffi::SSL_ERROR_SYSCALL => {
                    return Err(ssl_error("TLS handshake failed (syscall/connection error)"));
                }
                tls_ffi::SSL_ERROR_ZERO_RETURN => {
                    return Err(ssl_error("TLS handshake failed (peer closed connection)"));
                }
                tls_ffi::SSL_ERROR_SSL => {
                    return Err(ssl_error("TLS handshake failed (protocol error)"));
                }
                _ => {
                    return Err(ssl_error("TLS handshake failed"));
                }
            }
        }

        if self.verify {
            let result = unsafe { tls_ffi::SSL_get_verify_result(ssl.ssl) };
            if result != tls_ffi::X509_V_OK {
                return Err(Error::transport(TlsError(format!(
                    "certificate verification failed (X509 code {result})"
                ))));
            }
        }

        // Refuse to proceed unless the kernel actually took over crypto
        // in both directions; otherwise the zero-copy data path would be
        // silently wrong.
        let send_ok = unsafe { tls_ffi::ub_ssl_ktls_send_enabled(ssl.ssl) } == 1;
        let recv_ok = unsafe { tls_ffi::ub_ssl_ktls_recv_enabled(ssl.ssl) } == 1;
        if !send_ok || !recv_ok {
            return Err(Error::transport(TlsError(format!(
                "kernel TLS not engaged (tx={send_ok}, rx={recv_ok}); \
                 check kernel `tls` module and OpenSSL kTLS support"
            ))));
        }

        // The kernel now owns the crypto state on `fd`; the SSL object
        // is no longer needed and SSL_set_fd used a no-close BIO, so
        // dropping it here leaves `fd` (and kTLS) intact.
        Ok(())
    }
}

impl Drop for TlsContext {
    fn drop(&mut self) {
        // SAFETY: `ctx` was produced by SSL_CTX_new and is freed once.
        unsafe { tls_ffi::SSL_CTX_free(self.ctx) };
    }
}

/// Frees an `SSL_CTX` unless disarmed. Used to unwind a partially
/// configured context on error during `TlsContext::new`.
struct CtxGuard {
    ctx: *mut tls_ffi::SSL_CTX,
}

impl CtxGuard {
    fn disarm(self) {
        std::mem::forget(self);
    }
}

impl Drop for CtxGuard {
    fn drop(&mut self) {
        // SAFETY: only reached on the error path; ctx is a live SSL_CTX.
        unsafe { tls_ffi::SSL_CTX_free(self.ctx) };
    }
}

/// Frees an `SSL` on drop. The fd is owned elsewhere (no-close BIO).
struct SslGuard {
    ssl: *mut tls_ffi::SSL,
}

impl Drop for SslGuard {
    fn drop(&mut self) {
        // SAFETY: ssl is a live SSL object freed exactly once here.
        unsafe { tls_ffi::SSL_free(self.ssl) };
    }
}

fn set_nonblocking(fd: RawFd) -> Result<(), Error> {
    // SAFETY: standard fcntl get/set flags on a valid fd.
    unsafe {
        let flags = libc::fcntl(fd, libc::F_GETFL);
        if flags < 0 {
            return Err(io_error());
        }
        if libc::fcntl(fd, libc::F_SETFL, flags | libc::O_NONBLOCK) < 0 {
            return Err(io_error());
        }
    }
    Ok(())
}

fn io_error() -> Error {
    let e = std::io::Error::last_os_error();
    match e.raw_os_error() {
        Some(code) => Error::Io(code),
        None => Error::transport(e),
    }
}

/// Pull the top OpenSSL error off the thread-local queue and wrap it
/// with `context` for a human-legible message.
fn ssl_error(context: &str) -> Error {
    // SAFETY: ERR_get_error / ERR_error_string_n are thread-safe reads
    // of the OpenSSL error queue.
    let detail = unsafe {
        let code = tls_ffi::ERR_get_error();
        if code == 0 {
            String::new()
        } else {
            let mut buf = [0i8; 256];
            tls_ffi::ERR_error_string_n(code, buf.as_mut_ptr(), buf.len());
            let bytes = buf.iter().take_while(|&&c| c != 0).map(|&c| c as u8).collect::<Vec<u8>>();
            String::from_utf8_lossy(&bytes).into_owned()
        }
    };
    if detail.is_empty() {
        Error::transport(TlsError(context.to_string()))
    } else {
        Error::transport(TlsError(format!("{context}: {detail}")))
    }
}

#[derive(Debug)]
struct TlsError(String);

impl fmt::Display for TlsError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(&self.0)
    }
}

impl std::error::Error for TlsError {}
