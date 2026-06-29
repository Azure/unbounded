// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Client-side TLS context and handshake, built on OpenSSL 3 with
//! kernel TLS (kTLS).
//!
//! The handshake runs in userspace over the caller-owned socket fd, but
//! we set `SSL_OP_ENABLE_KTLS` so OpenSSL hands the negotiated keys to
//! the kernel (`setsockopt(SOL_TLS, ...)`). After a successful handshake
//! the kernel performs the symmetric crypto, so a caller keeps using
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
use std::rc::Rc;

use super::ffi;
use crate::bufferpool::Error;
use crate::ring::NetHandle;

/// Per-connection TLS knobs derived from the caller's config.
#[derive(Debug, Clone, Default)]
pub struct TlsConfig {
    /// Optional path to a PEM CA bundle to trust in addition to the
    /// system trust store. Mutually exclusive with `insecure_skip_verify`
    /// (rejected at config validation time).
    pub ca_cert_path: Option<String>,
    /// When true, skip certificate and hostname verification entirely.
    /// For test/private origins only.
    pub insecure_skip_verify: bool,
    /// Optional path to a PEM client certificate presented when an origin
    /// requires TLS client authentication.
    pub client_cert_path: Option<String>,
    /// Optional path to the PEM private key for `client_cert_path`.
    pub client_key_path: Option<String>,
}

/// A configured client `SSL_CTX`, shared by every connection a caller
/// makes. Shard-pinned: a `TlsContext` and the backend that owns it live
/// on one thread, so the raw `SSL_CTX` pointer is never touched
/// concurrently.
pub struct TlsContext {
    ctx: *mut ffi::SSL_CTX,
    verify: bool,
}

// SAFETY: the same shard-pinned justification as the backends. The
// `SSL_CTX` is only used from the single thread that owns the context;
// the Send + Sync bounds exist solely so a backend holding a
// `TlsContext` can satisfy the `Backend: Send + Sync` supertrait.
unsafe impl Send for TlsContext {}
unsafe impl Sync for TlsContext {}

impl TlsContext {
    /// Build a client TLS context from `config`.
    pub fn new(config: &TlsConfig) -> Result<Self, Error> {
        // SAFETY: OpenSSL 3 self-initializes on first use; every call
        // below is a standard libssl entry point with checked returns.
        unsafe {
            let method = ffi::TLS_client_method();
            if method.is_null() {
                return Err(ssl_error("TLS_client_method returned null"));
            }

            let ctx = ffi::SSL_CTX_new(method);
            if ctx.is_null() {
                return Err(ssl_error("SSL_CTX_new returned null"));
            }

            // From here on, free the ctx on any error.
            let guard = CtxGuard { ctx };

            if ffi::ub_ssl_ctx_set_min_proto_version(ctx, ffi::ub_tls1_2_version()) != 1 {
                return Err(ssl_error("SSL_CTX_set_min_proto_version failed"));
            }

            // Ask OpenSSL to enable kernel TLS for connections from this
            // context. The actual engagement is asserted post-handshake.
            ffi::ub_ssl_ctx_set_options(ctx, ffi::ub_ssl_op_enable_ktls());

            let verify = !config.insecure_skip_verify;
            if verify {
                ffi::SSL_CTX_set_verify(ctx, ffi::SSL_VERIFY_PEER, None);
                if ffi::SSL_CTX_set_default_verify_paths(ctx) != 1 {
                    return Err(ssl_error("SSL_CTX_set_default_verify_paths failed"));
                }
                if let Some(path) = config.ca_cert_path.as_deref() {
                    let c_path = CString::new(path).map_err(|_| {
                        Error::transport(TlsError("ca_cert_path contains NUL".into()))
                    })?;
                    if ffi::SSL_CTX_load_verify_locations(ctx, c_path.as_ptr(), std::ptr::null())
                        != 1
                    {
                        return Err(ssl_error("SSL_CTX_load_verify_locations failed"));
                    }
                }
            } else {
                ffi::SSL_CTX_set_verify(ctx, ffi::SSL_VERIFY_NONE, None);
            }

            match (
                config.client_cert_path.as_deref(),
                config.client_key_path.as_deref(),
            ) {
                (Some(cert), Some(key)) => {
                    let c_cert = CString::new(cert).map_err(|_| {
                        Error::transport(TlsError("client_cert_path contains NUL".into()))
                    })?;
                    let c_key = CString::new(key).map_err(|_| {
                        Error::transport(TlsError("client_key_path contains NUL".into()))
                    })?;
                    let filetype = ffi::ub_ssl_filetype_pem();
                    if ffi::SSL_CTX_use_certificate_file(ctx, c_cert.as_ptr(), filetype) != 1 {
                        return Err(ssl_error("SSL_CTX_use_certificate_file failed"));
                    }
                    if ffi::SSL_CTX_use_PrivateKey_file(ctx, c_key.as_ptr(), filetype) != 1 {
                        return Err(ssl_error("SSL_CTX_use_PrivateKey_file failed"));
                    }
                    if ffi::SSL_CTX_check_private_key(ctx) != 1 {
                        return Err(ssl_error("SSL_CTX_check_private_key failed"));
                    }
                }
                (None, None) => {}
                _ => {
                    return Err(Error::transport(TlsError(
                        "client_cert_path and client_key_path must be set together".into(),
                    )));
                }
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
        let ssl = unsafe { ffi::SSL_new(self.ctx) };
        if ssl.is_null() {
            return Err(ssl_error("SSL_new returned null"));
        }
        let ssl = SslGuard { ssl };

        // SAFETY: standard libssl client setup on a fresh SSL object.
        unsafe {
            if ffi::SSL_set_fd(ssl.ssl, fd) != 1 {
                return Err(ssl_error("SSL_set_fd failed"));
            }
            if ffi::ub_ssl_set_tlsext_host_name(ssl.ssl, c_host.as_ptr()) != 1 {
                return Err(ssl_error("SSL_set_tlsext_host_name failed"));
            }
            if self.verify && ffi::SSL_set1_host(ssl.ssl, c_host.as_ptr()) != 1 {
                return Err(ssl_error("SSL_set1_host failed"));
            }
            ffi::SSL_set_connect_state(ssl.ssl);
        }

        set_nonblocking(fd)?;

        loop {
            // OpenSSL requires the thread's error queue to be empty
            // before the I/O call so SSL_get_error classifies this
            // attempt's result reliably; stale entries from an earlier
            // handshake on this shard thread would otherwise be
            // misreported as a protocol error.
            unsafe { ffi::ERR_clear_error() };

            // SAFETY: ssl is valid; SSL_connect drives the handshake
            // against the non-blocking fd.
            let ret = unsafe { ffi::SSL_connect(ssl.ssl) };
            if ret == 1 {
                break;
            }
            let err = unsafe { ffi::SSL_get_error(ssl.ssl, ret) };
            match err {
                ffi::SSL_ERROR_WANT_READ => {
                    handle
                        .poll_ready(fd, libc::POLLIN as u32)
                        .await
                        .map_err(Error::transport)?;
                }
                ffi::SSL_ERROR_WANT_WRITE => {
                    handle
                        .poll_ready(fd, libc::POLLOUT as u32)
                        .await
                        .map_err(Error::transport)?;
                }
                ffi::SSL_ERROR_SYSCALL => {
                    return Err(ssl_error("TLS handshake failed (syscall/connection error)"));
                }
                ffi::SSL_ERROR_ZERO_RETURN => {
                    return Err(ssl_error("TLS handshake failed (peer closed connection)"));
                }
                ffi::SSL_ERROR_SSL => {
                    return Err(ssl_error("TLS handshake failed (protocol error)"));
                }
                _ => {
                    return Err(ssl_error("TLS handshake failed"));
                }
            }
        }

        if self.verify {
            let result = unsafe { ffi::SSL_get_verify_result(ssl.ssl) };
            if result != ffi::X509_V_OK {
                return Err(Error::transport(TlsError(format!(
                    "certificate verification failed (X509 code {result})"
                ))));
            }
        }

        // Refuse to proceed unless the kernel actually took over crypto
        // in both directions; otherwise the zero-copy data path would be
        // silently wrong.
        let send_ok = unsafe { ffi::ub_ssl_ktls_send_enabled(ssl.ssl) } == 1;
        let recv_ok = unsafe { ffi::ub_ssl_ktls_recv_enabled(ssl.ssl) } == 1;
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

/// Drive the TLS handshake when `tls` is set, returning whether the
/// connection is now TLS. A `None` context is a plaintext origin: no
/// handshake runs and `false` is returned. Centralizes the
/// `is_some()` + conditional-handshake pattern the origin backends
/// (`http`, `s3`, `azure`) repeat for every fetch.
pub(crate) async fn maybe_handshake(
    tls: &Option<Rc<TlsContext>>,
    handle: &NetHandle,
    fd: RawFd,
    sni_host: &str,
) -> Result<bool, Error> {
    if let Some(tls) = tls {
        tls.handshake(handle, fd, sni_host).await?;
        Ok(true)
    } else {
        Ok(false)
    }
}

impl Drop for TlsContext {
    fn drop(&mut self) {
        // SAFETY: `ctx` was produced by SSL_CTX_new and is freed once.
        unsafe { ffi::SSL_CTX_free(self.ctx) };
    }
}

/// Frees an `SSL_CTX` unless disarmed. Used to unwind a partially
/// configured context on error during `TlsContext::new`.
struct CtxGuard {
    ctx: *mut ffi::SSL_CTX,
}

impl CtxGuard {
    fn disarm(self) {
        std::mem::forget(self);
    }
}

impl Drop for CtxGuard {
    fn drop(&mut self) {
        // SAFETY: only reached on the error path; ctx is a live SSL_CTX.
        unsafe { ffi::SSL_CTX_free(self.ctx) };
    }
}

/// Frees an `SSL` on drop. The fd is owned elsewhere (no-close BIO).
struct SslGuard {
    ssl: *mut ffi::SSL,
}

impl Drop for SslGuard {
    fn drop(&mut self) {
        // SAFETY: ssl is a live SSL object freed exactly once here.
        unsafe { ffi::SSL_free(self.ssl) };
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
        let code = ffi::ERR_get_error();
        if code == 0 {
            String::new()
        } else {
            let mut buf = [0i8; 256];
            ffi::ERR_error_string_n(code, buf.as_mut_ptr(), buf.len());
            let bytes = buf
                .iter()
                .take_while(|&&c| c != 0)
                .map(|&c| c as u8)
                .collect::<Vec<u8>>();
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
