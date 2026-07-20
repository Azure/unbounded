// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Mutually authenticated TLS 1.3 contexts for peer connections.

use std::ffi::CString;
use std::fmt;
use std::os::fd::RawFd;

use super::ffi;
use crate::bufferpool::Error;
use crate::ring::NetHandle;

/// Local identity and trust roots for peer mutual TLS.
#[derive(Debug, Clone)]
pub struct PeerTlsConfig {
    /// PEM certificate chain presented to peers.
    pub cert_path: String,
    /// PEM private key corresponding to `cert_path`.
    pub key_path: String,
    /// PEM CA bundle used to authenticate peer certificates.
    pub ca_cert_path: String,
}

/// Paired client and server TLS contexts for authenticated peer connections.
pub struct PeerTlsContext {
    client_ctx: *mut ffi::SSL_CTX,
    server_ctx: *mut ffi::SSL_CTX,
}

impl PeerTlsContext {
    /// Build TLS 1.3-only client and server contexts from `config`.
    pub fn new(config: &PeerTlsConfig) -> Result<Self, Error> {
        let client_ctx = new_context(config, false)?;
        match new_context(config, true) {
            Ok(server_ctx) => Ok(Self {
                client_ctx,
                server_ctx,
            }),
            Err(error) => {
                // SAFETY: client_ctx was returned by SSL_CTX_new and has not
                // been transferred into a PeerTlsContext.
                unsafe { ffi::SSL_CTX_free(client_ctx) };
                Err(error)
            }
        }
    }

    /// Complete an authenticated client handshake over `fd`.
    ///
    /// `peer_hostname` is sent as SNI and must match a DNS subject alternative
    /// name in the authenticated server certificate. The legacy subject common
    /// name is never considered.
    pub async fn connect(
        &self,
        handle: &NetHandle,
        fd: RawFd,
        peer_hostname: &str,
    ) -> Result<(), Error> {
        let hostname = c_string(peer_hostname, "peer_hostname")?;
        let ssl = new_ssl(self.client_ctx)?;

        // SAFETY: ssl is fresh, hostname remains live through both calls, and
        // the caller owns fd for the duration of the handshake.
        unsafe {
            if ffi::SSL_set_fd(ssl.ssl, fd) != 1 {
                return Err(ssl_error("SSL_set_fd failed"));
            }
            if ffi::ub_ssl_set_tlsext_host_name(ssl.ssl, hostname.as_ptr()) != 1 {
                return Err(ssl_error("SSL_set_tlsext_host_name failed"));
            }
            ffi::ub_ssl_set_dns_san_only(ssl.ssl);
            if ffi::SSL_set1_host(ssl.ssl, hostname.as_ptr()) != 1 {
                return Err(ssl_error("SSL_set1_host failed"));
            }
            ffi::SSL_set_connect_state(ssl.ssl);
        }

        drive_handshake(handle, fd, &ssl, false).await?;
        verify_peer(ssl.ssl)?;
        require_ktls(ssl.ssl)
    }

    /// Complete an authenticated server handshake over `fd`.
    ///
    /// The client must present a certificate chaining to `ca_cert_path`. On
    /// success, the returned identity owns the authenticated certificate and
    /// can be matched against expected DNS SANs.
    pub async fn accept(
        &self,
        handle: &NetHandle,
        fd: RawFd,
    ) -> Result<PeerCertificateIdentity, Error> {
        let ssl = new_ssl(self.server_ctx)?;

        // SAFETY: ssl is fresh and the caller owns fd for the duration of the
        // handshake.
        unsafe {
            if ffi::SSL_set_fd(ssl.ssl, fd) != 1 {
                return Err(ssl_error("SSL_set_fd failed"));
            }
            ffi::SSL_set_accept_state(ssl.ssl);
        }

        drive_handshake(handle, fd, &ssl, true).await?;
        verify_peer(ssl.ssl)?;
        let identity = PeerCertificateIdentity::from_ssl(ssl.ssl)?;
        require_ktls(ssl.ssl)?;
        Ok(identity)
    }
}

impl Drop for PeerTlsContext {
    fn drop(&mut self) {
        // SAFETY: both pointers were returned by SSL_CTX_new and are each
        // released exactly once.
        unsafe {
            ffi::SSL_CTX_free(self.client_ctx);
            ffi::SSL_CTX_free(self.server_ctx);
        }
    }
}

/// Authenticated peer certificate identity returned by a server handshake.
pub struct PeerCertificateIdentity {
    cert: *mut ffi::X509,
    dns_sans: Vec<String>,
}

impl PeerCertificateIdentity {
    /// DNS subject alternative names carried by the peer certificate.
    pub fn dns_sans(&self) -> &[String] {
        &self.dns_sans
    }

    /// Match `hostname` against the certificate's DNS SANs using OpenSSL's
    /// RFC-compliant wildcard rules. The subject common name is never used.
    pub fn matches_dns_san(&self, hostname: &str) -> bool {
        let Ok(hostname) = CString::new(hostname) else {
            return false;
        };
        // SAFETY: cert is owned by self and hostname is NUL-terminated.
        unsafe { ffi::ub_x509_check_dns_san(self.cert, hostname.as_ptr()) == 1 }
    }

    fn from_ssl(ssl: *mut ffi::SSL) -> Result<Self, Error> {
        // SAFETY: ssl is live and get1 returns a new certificate reference.
        let cert = unsafe { ffi::ub_ssl_get1_peer_certificate(ssl) };
        if cert.is_null() {
            return Err(tls_error("peer did not present a certificate"));
        }
        let guard = CertGuard { cert };
        let dns_sans = read_dns_sans(cert)?;
        guard.disarm();
        Ok(Self { cert, dns_sans })
    }
}

impl fmt::Debug for PeerCertificateIdentity {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("PeerCertificateIdentity")
            .field("dns_sans", &self.dns_sans)
            .finish_non_exhaustive()
    }
}

impl Drop for PeerCertificateIdentity {
    fn drop(&mut self) {
        // SAFETY: cert is the owned reference returned by get1.
        unsafe { ffi::X509_free(self.cert) };
    }
}

fn new_context(config: &PeerTlsConfig, server: bool) -> Result<*mut ffi::SSL_CTX, Error> {
    let cert_path = c_string(&config.cert_path, "cert_path")?;
    let key_path = c_string(&config.key_path, "key_path")?;
    let ca_cert_path = c_string(&config.ca_cert_path, "ca_cert_path")?;

    // SAFETY: all OpenSSL return values are checked and guard owns the context
    // until it is explicitly disarmed.
    unsafe {
        let method = if server {
            ffi::TLS_server_method()
        } else {
            ffi::TLS_client_method()
        };
        if method.is_null() {
            return Err(ssl_error("TLS method returned null"));
        }
        let ctx = ffi::SSL_CTX_new(method);
        if ctx.is_null() {
            return Err(ssl_error("SSL_CTX_new returned null"));
        }
        let guard = CtxGuard { ctx };
        let tls13 = ffi::ub_tls1_3_version();
        if ffi::ub_ssl_ctx_set_min_proto_version(ctx, tls13) != 1
            || ffi::ub_ssl_ctx_set_max_proto_version(ctx, tls13) != 1
        {
            return Err(ssl_error("failed to require TLS 1.3"));
        }
        if server && ffi::ub_ssl_ctx_set_num_tickets(ctx, 0) != 1 {
            return Err(ssl_error("failed to disable peer TLS session tickets"));
        }
        ffi::ub_ssl_ctx_set_options(ctx, ffi::ub_ssl_op_enable_ktls());
        if ffi::SSL_CTX_load_verify_locations(ctx, ca_cert_path.as_ptr(), std::ptr::null()) != 1 {
            return Err(ssl_error("SSL_CTX_load_verify_locations failed"));
        }
        let mut verify_mode = ffi::SSL_VERIFY_PEER;
        if server {
            verify_mode |= ffi::SSL_VERIFY_FAIL_IF_NO_PEER_CERT;
        }
        ffi::SSL_CTX_set_verify(ctx, verify_mode, None);
        if ffi::SSL_CTX_use_certificate_chain_file(ctx, cert_path.as_ptr()) != 1 {
            return Err(ssl_error("SSL_CTX_use_certificate_chain_file failed"));
        }
        let filetype = ffi::ub_ssl_filetype_pem();
        if ffi::SSL_CTX_use_PrivateKey_file(ctx, key_path.as_ptr(), filetype) != 1 {
            return Err(ssl_error("loading peer private key failed"));
        }
        if ffi::SSL_CTX_check_private_key(ctx) != 1 {
            return Err(ssl_error("peer certificate/private key mismatch"));
        }
        guard.disarm();
        Ok(ctx)
    }
}

async fn drive_handshake(
    handle: &NetHandle,
    fd: RawFd,
    ssl: &SslGuard,
    server: bool,
) -> Result<(), Error> {
    set_nonblocking(fd)?;
    loop {
        // SAFETY: clearing the thread-local queue before each SSL I/O call is
        // required for reliable SSL_get_error classification.
        unsafe { ffi::ERR_clear_error() };
        // SAFETY: ssl is live and configured for the selected role.
        let ret = unsafe {
            if server {
                ffi::SSL_accept(ssl.ssl)
            } else {
                ffi::SSL_connect(ssl.ssl)
            }
        };
        if ret == 1 {
            return Ok(());
        }
        // SAFETY: ret is the result of the immediately preceding SSL call.
        match unsafe { ffi::SSL_get_error(ssl.ssl, ret) } {
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
                return Err(ssl_error(
                    "TLS handshake failed (protocol or certificate error)",
                ));
            }
            _ => return Err(ssl_error("TLS handshake failed")),
        }
    }
}

fn verify_peer(ssl: *mut ffi::SSL) -> Result<(), Error> {
    // SAFETY: ssl has completed a handshake.
    let result = unsafe { ffi::SSL_get_verify_result(ssl) };
    if result == ffi::X509_V_OK {
        Ok(())
    } else {
        Err(tls_error(format!(
            "certificate verification failed (X509 code {result})"
        )))
    }
}

fn require_ktls(ssl: *mut ffi::SSL) -> Result<(), Error> {
    // SAFETY: ssl has completed a handshake and owns both BIOs.
    let send_ok = unsafe { ffi::ub_ssl_ktls_send_enabled(ssl) } == 1;
    let recv_ok = unsafe { ffi::ub_ssl_ktls_recv_enabled(ssl) } == 1;
    if send_ok && recv_ok {
        Ok(())
    } else {
        Err(tls_error(format!(
            "kernel TLS not engaged (tx={send_ok}, rx={recv_ok}); check kernel `tls` module and OpenSSL kTLS support"
        )))
    }
}

fn read_dns_sans(cert: *mut ffi::X509) -> Result<Vec<String>, Error> {
    // SAFETY: cert is live for every shim call below.
    let count = unsafe { ffi::ub_x509_dns_san_count(cert) };
    if count < 0 {
        return Err(ssl_error("failed to read peer DNS SANs"));
    }
    let mut names = Vec::with_capacity(count as usize);
    for index in 0..count {
        // SAFETY: a null destination asks the shim for the required length.
        let len = unsafe { ffi::ub_x509_dns_san_copy(cert, index, std::ptr::null_mut(), 0) };
        if len < 0 {
            return Err(tls_error("peer certificate contains an invalid DNS SAN"));
        }
        let mut bytes = vec![0u8; len as usize + 1];
        // SAFETY: bytes has len + 1 capacity for the SAN and trailing NUL.
        let copied = unsafe {
            ffi::ub_x509_dns_san_copy(cert, index, bytes.as_mut_ptr().cast(), bytes.len())
        };
        if copied != len {
            return Err(tls_error("failed to copy peer DNS SAN"));
        }
        bytes.truncate(len as usize);
        let name = String::from_utf8(bytes)
            .map_err(|_| tls_error("peer certificate DNS SAN is not valid UTF-8"))?;
        names.push(name);
    }
    Ok(names)
}

fn new_ssl(ctx: *mut ffi::SSL_CTX) -> Result<SslGuard, Error> {
    // SAFETY: ctx remains owned by PeerTlsContext for the guard's lifetime.
    let ssl = unsafe { ffi::SSL_new(ctx) };
    if ssl.is_null() {
        Err(ssl_error("SSL_new returned null"))
    } else {
        Ok(SslGuard { ssl })
    }
}

fn c_string(value: &str, field: &str) -> Result<CString, Error> {
    CString::new(value).map_err(|_| tls_error(format!("{field} contains NUL")))
}

fn set_nonblocking(fd: RawFd) -> Result<(), Error> {
    // SAFETY: standard fcntl get/set flags on a caller-owned fd.
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
    let error = std::io::Error::last_os_error();
    match error.raw_os_error() {
        Some(code) => Error::Io(code),
        None => Error::transport(error),
    }
}

fn ssl_error(context: &str) -> Error {
    // SAFETY: these functions consume the calling thread's OpenSSL error queue.
    let detail = unsafe {
        let code = ffi::ERR_get_error();
        if code == 0 {
            String::new()
        } else {
            let mut buffer = [0 as std::ffi::c_char; 256];
            ffi::ERR_error_string_n(code, buffer.as_mut_ptr(), buffer.len());
            let bytes = buffer
                .iter()
                .take_while(|&&byte| byte != 0)
                .map(|&byte| byte as u8)
                .collect::<Vec<_>>();
            String::from_utf8_lossy(&bytes).into_owned()
        }
    };
    if detail.is_empty() {
        tls_error(context)
    } else {
        tls_error(format!("{context}: {detail}"))
    }
}

fn tls_error(message: impl Into<String>) -> Error {
    Error::transport(PeerTlsError(message.into()))
}

#[derive(Debug)]
struct PeerTlsError(String);

impl fmt::Display for PeerTlsError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(&self.0)
    }
}

impl std::error::Error for PeerTlsError {}

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
        // SAFETY: ctx is a live, uniquely owned SSL_CTX.
        unsafe { ffi::SSL_CTX_free(self.ctx) };
    }
}

struct SslGuard {
    ssl: *mut ffi::SSL,
}

impl Drop for SslGuard {
    fn drop(&mut self) {
        // SAFETY: ssl is a live, uniquely owned SSL object.
        unsafe { ffi::SSL_free(self.ssl) };
    }
}

struct CertGuard {
    cert: *mut ffi::X509,
}

impl CertGuard {
    fn disarm(self) {
        std::mem::forget(self);
    }
}

impl Drop for CertGuard {
    fn drop(&mut self) {
        // SAFETY: cert is the owned reference returned by get1.
        unsafe { ffi::X509_free(self.cert) };
    }
}

#[cfg(test)]
mod tests {
    use std::future::Future;
    use std::path::{Path, PathBuf};
    use std::process::Command;
    use std::rc::Rc;
    use std::task::{Context, Poll};

    use super::*;
    use crate::ring::NetworkRing;
    use crate::runtime::noop_waker;

    #[test]
    fn config_rejects_a_mismatched_private_key() {
        let Some(files) = TestCertificates::generate() else {
            return;
        };
        let config = PeerTlsConfig {
            cert_path: files.peer_cert().display().to_string(),
            key_path: files.ca_key().display().to_string(),
            ca_cert_path: files.ca_cert().display().to_string(),
        };
        let error = match PeerTlsContext::new(&config) {
            Ok(_) => panic!("mismatched key must fail"),
            Err(error) => error,
        };
        assert!(error.to_string().contains("private key"));
    }

    #[test]
    fn mtls_loopback_authenticates_dns_san() {
        let Some(files) = TestCertificates::generate() else {
            return;
        };
        let config = files.config();
        let context = PeerTlsContext::new(&config).expect("build peer TLS context");
        let ring = match NetworkRing::new(16) {
            Ok(ring) => Rc::new(ring),
            Err(error) => {
                eprintln!("peer TLS loopback: io_uring unavailable: {error}; skipping");
                return;
            }
        };
        let Some((client_fd, server_fd)) = tcp_loopback_pair() else {
            eprintln!("peer TLS loopback: TCP loopback unavailable; skipping");
            return;
        };
        let handle = NetHandle::new(Rc::clone(&ring));
        let client = context.connect(&handle, client_fd, "peer.test");
        let server = context.accept(&handle, server_fd);
        let (client_result, server_result) = block_on_two(&ring, client, server);
        close_pair(client_fd, server_fd);

        if ktls_unavailable(&client_result) || ktls_unavailable(&server_result) {
            eprintln!("peer TLS loopback: bidirectional kTLS unavailable; skipping");
            return;
        }
        client_result.expect("client handshake");
        let identity = server_result.expect("server handshake");
        assert_eq!(identity.dns_sans(), &["peer.test", "*.cluster.test"]);
        assert!(identity.matches_dns_san("peer.test"));
        assert!(identity.matches_dns_san("node.cluster.test"));
        assert!(!identity.matches_dns_san("other.test"));
        assert!(!identity.matches_dns_san("deep.node.cluster.test"));
    }

    fn block_on_two<F1, F2>(ring: &NetworkRing, first: F1, second: F2) -> (F1::Output, F2::Output)
    where
        F1: Future,
        F2: Future,
    {
        let waker = noop_waker();
        let mut task = Context::from_waker(&waker);
        let mut first = Box::pin(first);
        let mut second = Box::pin(second);
        let mut first_output = None;
        let mut second_output = None;
        let mut spins = 0u32;
        loop {
            if first_output.is_none()
                && let Poll::Ready(output) = first.as_mut().poll(&mut task)
            {
                first_output = Some(output);
            }
            if second_output.is_none()
                && let Poll::Ready(output) = second.as_mut().poll(&mut task)
            {
                second_output = Some(output);
            }
            if first_output.is_some() && second_output.is_some() {
                return (first_output.unwrap(), second_output.unwrap());
            }
            ring.progress();
            spins += 1;
            assert!(spins < 5_000_000, "peer TLS loopback made no progress");
        }
    }

    fn ktls_unavailable<T>(result: &Result<T, Error>) -> bool {
        result
            .as_ref()
            .err()
            .is_some_and(|error| error.to_string().contains("kernel TLS not engaged"))
    }

    fn tcp_loopback_pair() -> Option<(RawFd, RawFd)> {
        // SAFETY: all socket return values are checked and every failure path
        // closes the descriptors opened before it.
        unsafe {
            let listener = libc::socket(libc::AF_INET, libc::SOCK_STREAM, 0);
            if listener < 0 {
                return None;
            }
            let mut address: libc::sockaddr_in = std::mem::zeroed();
            address.sin_family = libc::AF_INET as libc::sa_family_t;
            address.sin_addr.s_addr = u32::to_be(0x7f00_0001);
            let address_len = std::mem::size_of::<libc::sockaddr_in>() as libc::socklen_t;
            if libc::bind(
                listener,
                &address as *const _ as *const libc::sockaddr,
                address_len,
            ) != 0
                || libc::listen(listener, 1) != 0
            {
                libc::close(listener);
                return None;
            }
            let mut bound: libc::sockaddr_in = std::mem::zeroed();
            let mut bound_len = address_len;
            if libc::getsockname(
                listener,
                &mut bound as *mut _ as *mut libc::sockaddr,
                &mut bound_len,
            ) != 0
            {
                libc::close(listener);
                return None;
            }
            let client = libc::socket(libc::AF_INET, libc::SOCK_STREAM, 0);
            if client < 0
                || libc::connect(
                    client,
                    &bound as *const _ as *const libc::sockaddr,
                    bound_len,
                ) != 0
            {
                if client >= 0 {
                    libc::close(client);
                }
                libc::close(listener);
                return None;
            }
            let server = libc::accept(listener, std::ptr::null_mut(), std::ptr::null_mut());
            libc::close(listener);
            if server < 0 {
                libc::close(client);
                None
            } else {
                Some((client, server))
            }
        }
    }

    fn close_pair(first: RawFd, second: RawFd) {
        // SAFETY: both descriptors are live and no handshake future remains.
        unsafe {
            libc::close(first);
            libc::close(second);
        }
    }

    struct TestCertificates {
        directory: tempfile::TempDir,
    }

    impl TestCertificates {
        fn generate() -> Option<Self> {
            let Some(openssl) = openssl_binary() else {
                eprintln!("peer TLS tests: bundled openssl unavailable; skipping");
                return None;
            };
            let directory = tempfile::tempdir().expect("create certificate directory");
            run_openssl(
                &openssl,
                directory.path(),
                &[
                    "req",
                    "-x509",
                    "-newkey",
                    "rsa:2048",
                    "-nodes",
                    "-keyout",
                    "ca.key",
                    "-out",
                    "ca.pem",
                    "-days",
                    "1",
                    "-subj",
                    "/CN=Peer Test CA",
                    "-addext",
                    "basicConstraints=critical,CA:TRUE",
                    "-addext",
                    "keyUsage=critical,keyCertSign,cRLSign",
                ],
            );
            run_openssl(
                &openssl,
                directory.path(),
                &[
                    "req",
                    "-new",
                    "-newkey",
                    "rsa:2048",
                    "-nodes",
                    "-keyout",
                    "peer.key",
                    "-out",
                    "peer.csr",
                    "-subj",
                    "/CN=ignored.test",
                    "-addext",
                    "subjectAltName=DNS:peer.test,DNS:*.cluster.test",
                    "-addext",
                    "extendedKeyUsage=serverAuth,clientAuth",
                ],
            );
            run_openssl(
                &openssl,
                directory.path(),
                &[
                    "x509",
                    "-req",
                    "-in",
                    "peer.csr",
                    "-CA",
                    "ca.pem",
                    "-CAkey",
                    "ca.key",
                    "-CAcreateserial",
                    "-out",
                    "peer.pem",
                    "-days",
                    "1",
                    "-sha256",
                    "-copy_extensions",
                    "copy",
                ],
            );
            Some(Self { directory })
        }

        fn config(&self) -> PeerTlsConfig {
            PeerTlsConfig {
                cert_path: self.peer_cert().display().to_string(),
                key_path: self.peer_key().display().to_string(),
                ca_cert_path: self.ca_cert().display().to_string(),
            }
        }

        fn peer_cert(&self) -> PathBuf {
            self.directory.path().join("peer.pem")
        }

        fn peer_key(&self) -> PathBuf {
            self.directory.path().join("peer.key")
        }

        fn ca_cert(&self) -> PathBuf {
            self.directory.path().join("ca.pem")
        }

        fn ca_key(&self) -> PathBuf {
            self.directory.path().join("ca.key")
        }
    }

    fn openssl_binary() -> Option<PathBuf> {
        let pkg_config = PathBuf::from(std::env::var_os("OPENSSL_PKG_CONFIG_PATH")?);
        let prefix = pkg_config.parent()?.parent()?;
        let binary = prefix.join("bin/openssl");
        binary.is_file().then_some(binary)
    }

    fn run_openssl(openssl: &Path, directory: &Path, arguments: &[&str]) {
        let output = Command::new(openssl)
            .current_dir(directory)
            .args(arguments)
            .output()
            .expect("run bundled openssl");
        assert!(
            output.status.success(),
            "openssl {:?} failed: {}",
            arguments,
            String::from_utf8_lossy(&output.stderr)
        );
    }
}
