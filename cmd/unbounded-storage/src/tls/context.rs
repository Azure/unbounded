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

use std::ffi::{CString, OsStr, OsString};
use std::fmt;
use std::os::fd::RawFd;
use std::os::unix::ffi::OsStrExt;
use std::path::{Path, PathBuf};
use std::rc::Rc;

use super::ffi;
use crate::bufferpool::Error;
use crate::ring::NetHandle;

/// Per-connection TLS knobs derived from the caller's config.
#[derive(Debug, Clone, Default)]
pub struct TlsConfig {
    /// Optional inline PEM CA bundle. When set, these certificates replace
    /// the host trust store.
    pub ca_cert: Option<String>,
    /// When true, skip certificate and hostname verification entirely.
    /// For test/private origins only.
    pub insecure_skip_verify: bool,
    /// Optional inline PEM client leaf certificate and additional chain.
    pub client_cert: Option<String>,
    /// Optional inline PEM private key for `client_cert`.
    pub client_key: Option<String>,
}

const HOST_CA_FILES: &[&str] = &[
    "/etc/ssl/certs/ca-certificates.crt",
    "/etc/pki/tls/certs/ca-bundle.crt",
    "/etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem",
    "/var/lib/ca-certificates/ca-bundle.pem",
    "/etc/pki/tls/cacert.pem",
    "/etc/ssl/ca-bundle.pem",
    "/etc/ssl/cert.pem",
];

const HOST_CA_DIRS: &[&str] = &["/etc/ssl/certs", "/etc/pki/tls/certs"];

/// A configured client `SSL_CTX`, shared by every connection a caller
/// makes. Shard-pinned: a `TlsContext` and the backend that owns it live
/// on one thread, so the raw `SSL_CTX` pointer is never touched
/// concurrently.
pub struct TlsContext {
    ctx: *mut ffi::SSL_CTX,
    verify: bool,
}

impl TlsContext {
    /// Build a client TLS context from `config`.
    pub fn new(config: &TlsConfig) -> Result<Self, Error> {
        validate_config(config)?;

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

            let tls12 = ffi::ub_tls1_2_version();
            if ffi::ub_ssl_ctx_set_min_proto_version(ctx, tls12) != 1 {
                return Err(ssl_error("SSL_CTX_set_min_proto_version failed"));
            }

            // Ask OpenSSL to enable kernel TLS for connections from this
            // context. The actual engagement is asserted post-handshake.
            ffi::ub_ssl_ctx_set_options(ctx, ffi::ub_ssl_op_enable_ktls());

            let verify = !config.insecure_skip_verify;
            if verify {
                ffi::SSL_CTX_set_verify(ctx, ffi::SSL_VERIFY_PEER, None);
                if let Some(pem) = config.ca_cert.as_deref() {
                    if ffi::ub_ssl_ctx_load_ca_pem(ctx, pem.as_ptr(), pem.len()) != 1 {
                        return Err(ssl_error("failed to parse inline CA certificate PEM"));
                    }
                } else {
                    load_host_roots(ctx)?;
                }
            } else {
                ffi::SSL_CTX_set_verify(ctx, ffi::SSL_VERIFY_NONE, None);
            }

            match (config.client_cert.as_deref(), config.client_key.as_deref()) {
                (Some(cert), Some(key)) => {
                    if ffi::ub_ssl_ctx_use_certificate_chain_pem(ctx, cert.as_ptr(), cert.len())
                        != 1
                    {
                        return Err(ssl_error("failed to parse inline client certificate PEM"));
                    }
                    if ffi::ub_ssl_ctx_use_private_key_pem(ctx, key.as_ptr(), key.len()) != 1 {
                        return Err(ssl_error("failed to parse inline client private key PEM"));
                    }
                    if ffi::SSL_CTX_check_private_key(ctx) != 1 {
                        return Err(ssl_error("client certificate and private key do not match"));
                    }
                }
                (None, None) => {}
                _ => {
                    return Err(Error::transport(TlsError(
                        "client_cert and client_key must be set together".into(),
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

#[derive(Debug, PartialEq, Eq)]
struct HostTrustLocations {
    file: Option<PathBuf>,
    dir: Option<OsString>,
}

fn validate_config(config: &TlsConfig) -> Result<(), Error> {
    if config.ca_cert.is_some() && config.insecure_skip_verify {
        return Err(tls_error(
            "ca_cert and insecure_skip_verify are mutually exclusive",
        ));
    }
    if config.client_cert.is_some() != config.client_key.is_some() {
        return Err(tls_error("client_cert and client_key must be set together"));
    }

    for (name, value) in [
        ("ca_cert", config.ca_cert.as_deref()),
        ("client_cert", config.client_cert.as_deref()),
        ("client_key", config.client_key.as_deref()),
    ] {
        if value.is_some_and(|pem| pem.trim().is_empty()) {
            return Err(tls_error(&format!("{name} must not be empty")));
        }
    }

    Ok(())
}

fn load_host_roots(ctx: *mut ffi::SSL_CTX) -> Result<(), Error> {
    let locations = resolve_host_trust_locations(
        std::env::var_os("SSL_CERT_FILE"),
        std::env::var_os("SSL_CERT_DIR"),
        Path::is_file,
        Path::is_dir,
    )?;

    if locations.file.is_none() && locations.dir.is_none() {
        // Keep a final fallback for distributions with non-standard paths.
        // This may use OPENSSLDIR, so it is attempted only after probing the
        // host paths that a bundled OpenSSL cannot discover itself.
        unsafe { ffi::ERR_clear_error() };
        if unsafe { ffi::SSL_CTX_set_default_verify_paths(ctx) } != 1 {
            return Err(ssl_error(
                "no host CA bundle or hashed directory found and OpenSSL defaults failed",
            ));
        }
        return Ok(());
    }

    let file = locations
        .file
        .as_deref()
        .map(|path| path_to_cstring(path.as_os_str(), "CA file path"))
        .transpose()?;
    let dir = locations
        .dir
        .as_deref()
        .map(|path| path_to_cstring(path, "CA directory path"))
        .transpose()?;

    unsafe { ffi::ERR_clear_error() };
    if unsafe {
        ffi::SSL_CTX_load_verify_locations(
            ctx,
            file.as_ref().map_or(std::ptr::null(), |path| path.as_ptr()),
            dir.as_ref().map_or(std::ptr::null(), |path| path.as_ptr()),
        )
    } != 1
    {
        return Err(ssl_error("failed to load host certificate trust"));
    }

    Ok(())
}

fn resolve_host_trust_locations<F, D>(
    ssl_cert_file: Option<OsString>,
    ssl_cert_dir: Option<OsString>,
    mut is_file: F,
    mut is_dir: D,
) -> Result<HostTrustLocations, Error>
where
    F: FnMut(&Path) -> bool,
    D: FnMut(&Path) -> bool,
{
    let file = match ssl_cert_file {
        Some(value) => {
            if value.as_bytes().is_empty() {
                return Err(tls_error("SSL_CERT_FILE must not be empty"));
            }
            let path = PathBuf::from(value);
            if !is_file(&path) {
                return Err(tls_error(&format!(
                    "SSL_CERT_FILE is not a regular file: {}",
                    path.display()
                )));
            }
            Some(path)
        }
        None => HOST_CA_FILES
            .iter()
            .map(PathBuf::from)
            .find(|path| is_file(path)),
    };

    let dir = match ssl_cert_dir {
        Some(value) => {
            validate_ca_dir_list(&value, &mut is_dir)?;
            Some(value)
        }
        None => HOST_CA_DIRS
            .iter()
            .map(PathBuf::from)
            .find(|path| is_dir(path))
            .map(PathBuf::into_os_string),
    };

    Ok(HostTrustLocations { file, dir })
}

fn validate_ca_dir_list<D>(value: &OsStr, is_dir: &mut D) -> Result<(), Error>
where
    D: FnMut(&Path) -> bool,
{
    if value.as_bytes().is_empty() {
        return Err(tls_error("SSL_CERT_DIR must not be empty"));
    }

    for bytes in value.as_bytes().split(|byte| *byte == b':') {
        if bytes.is_empty() {
            return Err(tls_error("SSL_CERT_DIR contains an empty directory"));
        }
        let path = Path::new(OsStr::from_bytes(bytes));
        if !is_dir(path) {
            return Err(tls_error(&format!(
                "SSL_CERT_DIR entry is not a directory: {}",
                path.display()
            )));
        }
    }

    Ok(())
}

fn path_to_cstring(path: &OsStr, name: &str) -> Result<CString, Error> {
    CString::new(path.as_bytes()).map_err(|_| tls_error(&format!("{name} contains NUL")))
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
            let mut buf = [0 as std::ffi::c_char; 256];
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

fn tls_error(message: &str) -> Error {
    Error::transport(TlsError(message.to_string()))
}

#[derive(Debug)]
struct TlsError(String);

impl fmt::Display for TlsError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(&self.0)
    }
}

impl std::error::Error for TlsError {}

#[cfg(test)]
mod tests {
    use super::*;

    const TEST_CERT: &str = "-----BEGIN CERTIFICATE-----\n\
MIIDDzCCAfegAwIBAgIUQsl2SwaDk66oMpVqFl3hYEOkKPEwDQYJKoZIhvcNAQEL\n\
BQAwFzEVMBMGA1UEAwwMc3RvcmFnZS10ZXN0MB4XDTI2MDcxNzIwMjAxMVoXDTI2\n\
MDcxODIwMjAxMVowFzEVMBMGA1UEAwwMc3RvcmFnZS10ZXN0MIIBIjANBgkqhkiG\n\
9w0BAQEFAAOCAQ8AMIIBCgKCAQEAsFh5KbDC9AK8PfwLC1rhjy2wxJhJHbXIfD79\n\
l/F+uXOdSQZTK4MS/qtZt6sTChfts2Auxe8+xzX33z8310E9VHu5T7BfljhjEr2U\n\
aMLRuzZxiKGhA1IloQGzqsNoxsvV+IJ97j86IaUU6SqD0DuIEC4SvDFhM0mgt13X\n\
Y15pkmadKYwHmf8VRHO79dfAL5Tdkbijn4oGVcjTudOGKVvCyZ59zqKbwKN4UWQM\n\
ajU51o04OZfzGV6bhN0BJydO90EM4RGvVleGMZBoyMiITjscGCLSCsJsFs9aMAYE\n\
AWCUIp+vRofutRUztcK6O1rhLZ1duOBAO7LWwgkuDu4X2b5u7wIDAQABo1MwUTAd\n\
BgNVHQ4EFgQU1WiQf8joyXMZ689kmbNLK6g8c0MwHwYDVR0jBBgwFoAU1WiQf8jo\n\
yXMZ689kmbNLK6g8c0MwDwYDVR0TAQH/BAUwAwEB/zANBgkqhkiG9w0BAQsFAAOC\n\
AQEAJLGbs7HlspZJvBWGvo8w/5cJXH+qPvLjBGugULxDCLryn8tpnVRmr1gOpJWj\n\
tvWOBA5O6d9jnHPoLUAk0Jtzh67B/kDcAc44poxIDqsvSfVbAln5UqG9vih+TFVo\n\
G6jTKkt3q63e8ahLJwRe0qa61IU0lA2j0zBAcwrK++iGOezyuAKUeo0Vwk5BGsSa\n\
ilIOWfp1I0ySgE0InRH8rTTORAMtY6nKfKmkuF6m2CYKXnaI/VqF01GEOPZBnHoo\n\
Spy+sYgDRl+cTmrhsUrCHdt/P6K7CaMuIf/gn/Imh4kN+R0TpWmfdkW7B+IREN1G\n\
KMjAx6YCVh/vMzoLdoypKqR9hw==\n\
-----END CERTIFICATE-----\n";

    const TEST_KEY: &str = "-----BEGIN PRIVATE KEY-----\n\
MIIEvgIBADANBgkqhkiG9w0BAQEFAASCBKgwggSkAgEAAoIBAQCwWHkpsML0Arw9\n\
/AsLWuGPLbDEmEkdtch8Pv2X8X65c51JBlMrgxL+q1m3qxMKF+2zYC7F7z7HNfff\n\
PzfXQT1Ue7lPsF+WOGMSvZRowtG7NnGIoaEDUiWhAbOqw2jGy9X4gn3uPzohpRTp\n\
KoPQO4gQLhK8MWEzSaC3XddjXmmSZp0pjAeZ/xVEc7v118AvlN2RuKOfigZVyNO5\n\
04YpW8LJnn3OopvAo3hRZAxqNTnWjTg5l/MZXpuE3QEnJ073QQzhEa9WV4YxkGjI\n\
yIhOOxwYItIKwmwWz1owBgQBYJQin69Gh+61FTO1wro7WuEtnV244EA7stbCCS4O\n\
7hfZvm7vAgMBAAECggEAF51BXFvXP2W+X26I7BRXcBzmNu1NnTTijADDZL1qAtuA\n\
jG7UZFdBC+lWMkouWoOpyQNwQAExnuuTLcoBaEnMNKv8vLcZlbwnSDMq1HyCKVe5\n\
DFrYfOFbOJxJuuw/858IICcZRfYhiq/YhQC0dgYCymfhCmJyabPKWcOvPBdAe+IY\n\
wxOg0Lk5xQJYGlYir3J1C6e38RgrhEmbr9NvaO2MI5qguvYayg58QB9Ug95mtJkf\n\
T9nxquj1Ts4kSuYDCeZtdGLNUB0nlEEOUU0AO65z7+a1otiXeX0Yj56AnuWvOX+4\n\
hD3XGSfynfUjKpW+jLVgZZ3VthooBr+51yJDS4roAQKBgQDmOPJMbdHDJAX/Nrq/\n\
eM1UkkFIqhD/PHBV9RBsqJwHuZip62ah7M1iSuBKMXG/ogLglB+Q8f/rcYsi1f02\n\
R+F0Hf/WZe0/5rhC8tqanFAtL31EgSHitaLfbRheW204Pp4YwY2AeHHW+Z0KIhD6\n\
9ihqV7ZsByKf3pL2nnJmbx5JgQKBgQDEFzX4p1Vi+vhwiI684CsM50pxzgfx91vC\n\
VfCgugMGZ82xp0s1zBkcXevv3iIl35ndGh2PhKOGE6+XHFtldttMXrdAs29q463V\n\
fWNEBQiU4N5VBKlWNlW5odFnLsBOnNxsobrbtrcWjbcKayw6n2wq1TJdxmtlxKo9\n\
ToaXHESQbwKBgQC8tDS2vNVQ1DguJtgPlZ8IERF91Bg2fX2+ly6tQc8S7efab18i\n\
no0CYklRxxFreApPtlnhXtrcS6c2GJyCX4zGtsg7HjTHSgACsDjKvhFh2Ckfe5Eg\n\
2Kz14eA1h08Q6RKBTDUF9rOo99Tmt2GfsyEReW/HQFn7HF7t0pYGrFHxAQKBgQC1\n\
pFqWb0slWR3yAE1YoL7AQTAwo42wklYperpf6G8M6/MaccG1n85S/J2loLs5IhvB\n\
OIPRgiiH9oxdCiOPpb4WzFYsVQsMlMNeU7w0MgV1A6hwUNUby1E1l7QGRMRXDe8R\n\
oe8Zv/NxrOy1dfmOhEcKllsFitvJdZfNGoSKTeEleQKBgFXIgwBzyLfiHRJGpmR0\n\
dK68dKQskMoi0+P3DkuPbYPt4+r40dsCxi6Ior7i5+siuKEYf+boYEaa6oLucC2v\n\
QL/z3kv6kTv8rl+Ltxnd0zvwg0aCW1G80/xR7kdCR37JqzleKjBHVaNEPqK47e/Y\n\
gkCxP7WO2UaJv3eZFti4iPPm\n\
-----END PRIVATE KEY-----\n";

    #[test]
    fn inline_ca_and_insecure_are_mutually_exclusive() {
        let config = TlsConfig {
            ca_cert: Some("certificate".into()),
            insecure_skip_verify: true,
            ..TlsConfig::default()
        };

        let error = validate_config(&config).unwrap_err();
        assert!(error.to_string().contains("mutually exclusive"));
    }

    #[test]
    fn client_certificate_and_key_must_be_paired() {
        let config = TlsConfig {
            client_cert: Some("certificate".into()),
            ..TlsConfig::default()
        };

        let error = validate_config(&config).unwrap_err();
        assert!(error.to_string().contains("must be set together"));
    }

    #[test]
    fn inline_pem_must_not_be_empty() {
        let config = TlsConfig {
            ca_cert: Some(" \n\t".into()),
            ..TlsConfig::default()
        };

        let error = validate_config(&config).unwrap_err();
        assert!(error.to_string().contains("ca_cert must not be empty"));
    }

    #[test]
    fn environment_trust_paths_override_standard_locations() {
        let file = OsString::from("/custom/ca.pem");
        let dirs = OsString::from("/custom/certs:/other/certs");
        let locations = resolve_host_trust_locations(
            Some(file.clone()),
            Some(dirs.clone()),
            |path| path == Path::new("/custom/ca.pem"),
            |path| path == Path::new("/custom/certs") || path == Path::new("/other/certs"),
        )
        .unwrap();

        assert_eq!(
            locations,
            HostTrustLocations {
                file: Some(PathBuf::from(file)),
                dir: Some(dirs),
            }
        );
    }

    #[test]
    fn standard_locations_fill_unset_environment_paths() {
        let locations = resolve_host_trust_locations(
            None,
            None,
            |path| path == Path::new(HOST_CA_FILES[1]),
            |path| path == Path::new(HOST_CA_DIRS[1]),
        )
        .unwrap();

        assert_eq!(locations.file, Some(PathBuf::from(HOST_CA_FILES[1])));
        assert_eq!(locations.dir, Some(OsString::from(HOST_CA_DIRS[1])));
    }

    #[test]
    fn malformed_inline_ca_pem_is_rejected() {
        let config = TlsConfig {
            ca_cert: Some("not a PEM certificate".into()),
            ..TlsConfig::default()
        };

        let error = match TlsContext::new(&config) {
            Ok(_) => panic!("malformed CA PEM was accepted"),
            Err(error) => error,
        };
        assert!(error.to_string().contains("inline CA certificate PEM"));
    }

    #[test]
    fn malformed_inline_client_pem_is_rejected() {
        let config = TlsConfig {
            insecure_skip_verify: true,
            client_cert: Some("not a PEM certificate".into()),
            client_key: Some("not a PEM key".into()),
            ..TlsConfig::default()
        };

        let error = match TlsContext::new(&config) {
            Ok(_) => panic!("malformed client certificate PEM was accepted"),
            Err(error) => error,
        };
        assert!(error.to_string().contains("inline client certificate PEM"));
    }

    #[test]
    fn inline_ca_rejects_trailing_non_whitespace() {
        TlsContext::new(&TlsConfig {
            ca_cert: Some(format!("{TEST_CERT} \n\t")),
            ..TlsConfig::default()
        })
        .expect("trailing whitespace should be accepted");

        let error = match TlsContext::new(&TlsConfig {
            ca_cert: Some(format!("{TEST_CERT}garbage")),
            ..TlsConfig::default()
        }) {
            Ok(_) => panic!("trailing CA material was accepted"),
            Err(error) => error,
        };
        assert!(error.to_string().contains("inline CA certificate PEM"));
    }

    #[test]
    fn inline_private_key_rejects_trailing_non_whitespace() {
        TlsContext::new(&TlsConfig {
            insecure_skip_verify: true,
            client_cert: Some(TEST_CERT.into()),
            client_key: Some(format!("{TEST_KEY} \n\t")),
            ..TlsConfig::default()
        })
        .expect("trailing whitespace should be accepted");

        let error = match TlsContext::new(&TlsConfig {
            insecure_skip_verify: true,
            client_cert: Some(TEST_CERT.into()),
            client_key: Some(format!("{TEST_KEY}{TEST_KEY}")),
            ..TlsConfig::default()
        }) {
            Ok(_) => panic!("additional private key was accepted"),
            Err(error) => error,
        };
        assert!(error.to_string().contains("inline client private key PEM"));
    }
}
