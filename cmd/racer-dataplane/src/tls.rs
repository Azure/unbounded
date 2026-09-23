// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! TLS 1.3 over OpenSSL's native socket BIO, including negotiated Linux kTLS.
//!
//! A session owns its socket. After a WANT result, poll the indicated direction
//! and retry the operation. Application bytes must never bypass this session.

mod channel;
pub use channel::TlsChannel;

use foreign_types::ForeignType;
use openssl::{
    asn1::Asn1Time,
    ec::{EcGroup, EcKey},
    hash::MessageDigest,
    nid::Nid,
    pkey::PKey,
    ssl::{Ssl, SslContext, SslContextBuilder, SslMethod, SslVerifyMode, SslVersion},
    stack::Stack,
    x509::{
        X509, X509NameBuilder, X509PurposeId, X509Req, X509StoreContext,
        extension::SubjectAlternativeName,
        store::X509StoreBuilder,
        verify::{X509CheckFlags, X509VerifyFlags},
    },
};
use serde::Deserialize;
use sha2::{Digest, Sha256};
use std::{
    ffi::c_void,
    io,
    os::fd::{AsRawFd, BorrowedFd, OwnedFd, RawFd},
    path::Path,
    sync::atomic::{AtomicU64, Ordering},
};

const MAX_BUNDLE_BYTES: usize = 1024 * 1024;
const WRITE_CHUNK: usize = 64 * 1024;
const CONTROL_PLANE_URI: &str = "spiffe://racer/controlplane";

/// Application admission lease, independent of record I/O. Accepted transfers
/// keep their own deadlines even after certificate expiry or the connection cap.
#[derive(Clone, Copy)]
pub(crate) struct Admission {
    created: std::time::Instant,
    pub(crate) expires_unix: u64,
}
impl Admission {
    pub(crate) fn new(expires_unix: u64) -> Self {
        Self {
            created: crate::environment::now(),
            expires_unix,
        }
    }
    pub(crate) fn expired(&self, peer_limit: u64) -> bool {
        crate::environment::now().saturating_duration_since(self.created)
            >= std::time::Duration::from_secs(300)
            || crate::environment::wall()
                .duration_since(std::time::UNIX_EPOCH)
                .unwrap_or_default()
                .as_secs()
                >= self.expires_unix.min(peer_limit)
    }
}

fn invalid(message: impl Into<String>) -> io::Error {
    io::Error::new(io::ErrorKind::InvalidData, message.into())
}

fn ssl_error(error: openssl::error::ErrorStack) -> io::Error {
    io::Error::other(error)
}

fn hex(bytes: &[u8]) -> String {
    const DIGITS: &[u8] = b"0123456789abcdef";
    bytes
        .iter()
        .flat_map(|b| {
            [
                DIGITS[(b >> 4) as usize] as char,
                DIGITS[(b & 15) as usize] as char,
            ]
        })
        .collect()
}

fn is_id(value: &str) -> bool {
    value.len() == 64
        && value
            .bytes()
            .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b))
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct PeerIdentity {
    pub universe: String,
    pub node: String,
    pub pod_uid: String,
}

impl PeerIdentity {
    pub fn new(universe: &str, node: &str, pod_uid: &str) -> io::Result<Self> {
        if !is_id(universe)
            || !is_id(node)
            || pod_uid.is_empty()
            || pod_uid.len() > 253
            || !pod_uid
                .bytes()
                .all(|b| b.is_ascii_alphanumeric() || b == b'-')
        {
            return Err(invalid("invalid Racer certificate identity"));
        }
        Ok(Self {
            universe: universe.into(),
            node: node.into(),
            pod_uid: pod_uid.into(),
        })
    }

    pub fn uri(&self) -> String {
        format!(
            "spiffe://racer/universe/{}/node/{}/pod/{}",
            self.universe, self.node, self.pod_uid
        )
    }

    pub fn parse(uri: &str) -> io::Result<Self> {
        let parts: Vec<_> = uri.split('/').collect();
        if parts.len() != 9
            || parts[0] != "spiffe:"
            || parts[1] != ""
            || parts[2] != "racer"
            || parts[3] != "universe"
            || parts[5] != "node"
            || parts[7] != "pod"
        {
            return Err(invalid("invalid Racer URI SAN"));
        }
        Self::new(parts[4], parts[6], parts[8])
    }
}

#[derive(Clone, Debug)]
pub enum ExpectedPeer {
    Identity(PeerIdentity),
    ControlPlane {
        dns_name: String,
    },
    /// Inbound authentication before HTTP routing; authorize the returned node
    /// and pod identity against the current topology before serving a request.
    Universe(String),
}

#[derive(Clone)]
pub struct TrustBundle {
    pub generation: u64,
    pub active: String,
    pub digest: [u8; 32],
    certificates: Vec<X509>,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct BundleJson {
    version: u32,
    generation: u64,
    active: String,
    certificates: String,
}

fn certificates(pem: &[u8]) -> io::Result<Vec<X509>> {
    // OpenSSL's stack parser silently ignores non-PEM suffixes. Reject them so
    // mounted trust material has one unambiguous interpretation.
    let text = std::str::from_utf8(pem).map_err(|_| invalid("certificate PEM is not UTF-8"))?;
    let mut remaining = text.trim();
    let mut result = Vec::new();
    while !remaining.is_empty() {
        if !remaining.starts_with("-----BEGIN CERTIFICATE-----") {
            return Err(invalid("unexpected data in certificate PEM"));
        }
        let end = remaining
            .find("-----END CERTIFICATE-----")
            .ok_or_else(|| invalid("unterminated certificate PEM"))?
            + "-----END CERTIFICATE-----".len();
        result.push(X509::from_pem(remaining[..end].as_bytes()).map_err(ssl_error)?);
        remaining = remaining[end..].trim();
    }
    if result.is_empty() {
        return Err(invalid("empty certificate chain"));
    }
    Ok(result)
}

impl TrustBundle {
    pub fn parse(bytes: &[u8], previous: Option<&Self>) -> io::Result<Self> {
        if bytes.len() > MAX_BUNDLE_BYTES {
            return Err(invalid("trust bundle is too large"));
        }
        let wire: BundleJson = serde_json::from_slice(bytes).map_err(invalid_json)?;
        if wire.generation == 0 {
            return Err(invalid("trust bundle generation must be positive"));
        }
        if wire.version != 1 || !is_id(&wire.active) {
            return Err(invalid(
                "unsupported trust bundle version or active root digest",
            ));
        }
        let digest: [u8; 32] = Sha256::digest(bytes).into();
        if let Some(previous) = previous {
            if wire.generation < previous.generation {
                return Err(invalid("trust bundle generation rollback"));
            }
            if wire.generation == previous.generation && digest != previous.digest {
                return Err(invalid("trust bundle generation equivocation"));
            }
        }
        let certificates = certificates(wire.certificates.as_bytes())?;
        if certificates.len() > 2 {
            return Err(invalid("trust bundle must contain at most two roots"));
        }
        let mut roots = std::collections::HashSet::new();
        for cert in &certificates {
            let public_key = cert.public_key().map_err(ssl_error)?;
            if unsafe { racer_tls_is_ca(cert.as_ptr().cast()) } != 1
                || cert.subject_name().to_der().map_err(ssl_error)?
                    != cert.issuer_name().to_der().map_err(ssl_error)?
                || !cert.verify(&public_key).map_err(ssl_error)?
            {
                return Err(invalid("trust bundle contains a non-root certificate"));
            }
            let digest = hex(&Sha256::digest(cert.to_der().map_err(ssl_error)?));
            if !roots.insert(digest) {
                return Err(invalid("duplicate trust root"));
            }
        }
        if !roots.contains(&wire.active) {
            return Err(invalid("active root is absent from trust bundle"));
        }
        Ok(Self {
            generation: wire.generation,
            active: wire.active,
            digest,
            certificates,
        })
    }

    pub fn load(directory: &Path, previous: Option<&Self>) -> io::Result<Self> {
        use io::Read;
        let mut bytes = Vec::new();
        std::fs::File::open(directory.join("bundle.json"))?
            .take((MAX_BUNDLE_BYTES + 1) as u64)
            .read_to_end(&mut bytes)?;
        Self::parse(&bytes, previous)
    }

    pub fn certificates(&self) -> &[X509] {
        &self.certificates
    }

    fn store(&self) -> io::Result<openssl::x509::store::X509Store> {
        let mut builder = X509StoreBuilder::new().map_err(ssl_error)?;
        builder
            .set_flags(X509VerifyFlags::X509_STRICT | X509VerifyFlags::CHECK_SS_SIGNATURE)
            .map_err(ssl_error)?;
        for cert in &self.certificates {
            builder.add_cert(cert.clone()).map_err(ssl_error)?;
        }
        Ok(builder.build())
    }
}

fn invalid_json(error: serde_json::Error) -> io::Error {
    invalid(error.to_string())
}

pub struct IdentityRequest {
    pub private_key_pem: Vec<u8>,
    pub csr_pem: Vec<u8>,
}

pub fn generate_key_and_csr(identity: &PeerIdentity) -> io::Result<IdentityRequest> {
    PeerIdentity::new(&identity.universe, &identity.node, &identity.pod_uid)?;
    let group = EcGroup::from_curve_name(Nid::X9_62_PRIME256V1).map_err(ssl_error)?;
    let key = PKey::from_ec_key(EcKey::generate(&group).map_err(ssl_error)?).map_err(ssl_error)?;
    let mut request = X509Req::builder().map_err(ssl_error)?;
    request.set_version(0).map_err(ssl_error)?;
    let name = X509NameBuilder::new().map_err(ssl_error)?.build();
    request.set_subject_name(&name).map_err(ssl_error)?;
    request.set_pubkey(&key).map_err(ssl_error)?;
    let san = SubjectAlternativeName::new()
        .uri(&identity.uri())
        .build(&request.x509v3_context(None))
        .map_err(ssl_error)?;
    let mut extensions = Stack::new().map_err(ssl_error)?;
    extensions.push(san).map_err(ssl_error)?;
    request.add_extensions(&extensions).map_err(ssl_error)?;
    request
        .sign(&key, MessageDigest::sha256())
        .map_err(ssl_error)?;
    Ok(IdentityRequest {
        private_key_pem: key.private_key_to_pem_pkcs8().map_err(ssl_error)?,
        csr_pem: request.build().to_pem().map_err(ssl_error)?,
    })
}

fn uri_san(cert: &X509) -> io::Result<String> {
    let sans = cert
        .subject_alt_names()
        .ok_or_else(|| invalid("peer certificate lacks SAN"))?;
    let uris: Vec<_> = sans.iter().filter_map(|san| san.uri()).collect();
    if uris.len() != 1 {
        return Err(invalid("peer certificate must have exactly one URI SAN"));
    }
    Ok(uris[0].into())
}

fn timestamp(time: &openssl::asn1::Asn1TimeRef) -> io::Result<u64> {
    let epoch = Asn1Time::from_unix(0).map_err(ssl_error)?;
    let delta = epoch.diff(time).map_err(ssl_error)?;
    let seconds = i64::from(delta.days) * 86400 + i64::from(delta.secs);
    u64::try_from(seconds).map_err(|_| invalid("certificate expiry predates epoch"))
}

fn expiry(cert: &X509) -> io::Result<u64> {
    timestamp(cert.not_after())
}

pub fn leaf_expiry_unix(certificate_pem: &[u8]) -> io::Result<u64> {
    expiry(&certificates(certificate_pem)?[0])
}

#[derive(Clone, Debug)]
pub struct LeafInfo {
    pub identity: PeerIdentity,
    pub issuer: String,
    pub issued_unix: u64,
    pub expires_unix: u64,
}

pub fn validate_leaf(
    bundle: &TrustBundle,
    certificate_pem: &[u8],
    private_key_pem: &[u8],
    expected: &PeerIdentity,
) -> io::Result<LeafInfo> {
    let certs = certificates(certificate_pem)?;
    let leaf = &certs[0];
    let key = PKey::private_key_from_pem(private_key_pem).map_err(ssl_error)?;
    if !leaf.public_key().map_err(ssl_error)?.public_eq(&key) {
        return Err(invalid("leaf certificate does not match private key"));
    }
    if unsafe { racer_tls_is_ca(leaf.as_ptr().cast()) } != 0 {
        return Err(invalid("identity certificate is a CA"));
    }
    let identity = PeerIdentity::parse(&uri_san(leaf)?)?;
    if &identity != expected {
        return Err(invalid("leaf certificate identity mismatch"));
    }
    let mut chain = Stack::new().map_err(ssl_error)?;
    for cert in certs.iter().skip(1) {
        chain.push(cert.clone()).map_err(ssl_error)?;
    }
    let mut issuer = None;
    for purpose in [X509PurposeId::SSL_CLIENT, X509PurposeId::SSL_SERVER] {
        let mut store = X509StoreBuilder::new().map_err(ssl_error)?;
        store
            .set_flags(X509VerifyFlags::X509_STRICT | X509VerifyFlags::CHECK_SS_SIGNATURE)
            .map_err(ssl_error)?;
        store.set_purpose(purpose).map_err(ssl_error)?;
        for root in bundle.certificates() {
            store.add_cert(root.clone()).map_err(ssl_error)?;
        }
        let store = store.build();
        let mut verify = X509StoreContext::new().map_err(ssl_error)?;
        if !verify
            .init(&store, leaf, &chain, |ctx| {
                let valid = ctx.verify_cert()?;
                if valid {
                    if let Some(root) = ctx.chain().and_then(|chain| chain.iter().last()) {
                        issuer = Some(hex(&Sha256::digest(root.to_der()?)));
                    }
                }
                Ok(valid)
            })
            .map_err(ssl_error)?
        {
            return Err(invalid(format!(
                "invalid leaf certificate: {}",
                verify.error()
            )));
        }
    }
    Ok(LeafInfo {
        identity,
        issuer: issuer.ok_or_else(|| invalid("verified chain has no root"))?,
        issued_unix: timestamp(leaf.not_before())?,
        expires_unix: expiry(leaf)?,
    })
}

#[derive(Clone)]
pub struct TlsContext {
    context: SslContext,
    has_identity: bool,
    local_expiry_unix: Option<u64>,
}

impl TlsContext {
    pub fn new(
        bundle: &TrustBundle,
        certificate_pem: &[u8],
        private_key_pem: &[u8],
    ) -> io::Result<Self> {
        Self::build(bundle, Some((certificate_pem, private_key_pem)), true)
    }

    /// Only for enrollment. Peer/server verification remains mandatory.
    pub fn bootstrap(bundle: &TrustBundle) -> io::Result<Self> {
        Self::build(bundle, None, true)
    }

    fn build(
        bundle: &TrustBundle,
        identity: Option<(&[u8], &[u8])>,
        ktls: bool,
    ) -> io::Result<Self> {
        let mut builder = SslContextBuilder::new(SslMethod::tls()).map_err(ssl_error)?;
        builder
            .set_min_proto_version(Some(SslVersion::TLS1_3))
            .map_err(ssl_error)?;
        builder
            .set_max_proto_version(Some(SslVersion::TLS1_3))
            .map_err(ssl_error)?;
        builder.set_verify(SslVerifyMode::PEER | SslVerifyMode::FAIL_IF_NO_PEER_CERT);
        builder.set_verify_depth(4);
        builder.set_cert_store(bundle.store()?);
        // Prefer AES-GCM supported by kTLS; allow ChaCha20 for encrypted fallback.
        builder
            .set_ciphersuites(
                "TLS_AES_256_GCM_SHA384:TLS_AES_128_GCM_SHA256:TLS_CHACHA20_POLY1305_SHA256",
            )
            .map_err(ssl_error)?;
        if unsafe { racer_tls_configure(builder.as_ptr().cast(), i32::from(ktls)) } != 1 {
            return Err(ssl_error(openssl::error::ErrorStack::get()));
        }
        let mut local_expiry_unix = None;
        if let Some((certificate_pem, private_key_pem)) = identity {
            let mut certs = certificates(certificate_pem)?.into_iter();
            let leaf = certs.next().unwrap();
            local_expiry_unix = Some(expiry(&leaf)?);
            builder.set_certificate(&leaf).map_err(ssl_error)?;
            for cert in certs {
                builder.add_extra_chain_cert(cert).map_err(ssl_error)?;
            }
            let key = PKey::private_key_from_pem(private_key_pem).map_err(ssl_error)?;
            builder.set_private_key(&key).map_err(ssl_error)?;
            builder.check_private_key().map_err(ssl_error)?;
        }
        Ok(Self {
            context: builder.build(),
            has_identity: identity.is_some(),
            local_expiry_unix,
        })
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum TlsProgress<T> {
    Complete(T),
    WantRead,
    WantWrite,
    Eof,
}

#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub struct Offload {
    pub tx: bool,
    pub rx: bool,
}

#[derive(Clone, Copy, Debug, Default)]
pub struct TlsCounters {
    pub handshakes: u64,
    pub ktls_tx_connections: u64,
    pub ktls_rx_connections: u64,
    pub encrypted_fallback_connections: u64,
    pub tx_bytes: u64,
    pub rx_bytes: u64,
    pub sendfile_bytes: u64,
    pub fallback_sendfile_bytes: u64,
}

static COUNTERS: [AtomicU64; 8] = [const { AtomicU64::new(0) }; 8];

pub fn global_counters() -> TlsCounters {
    let values = COUNTERS
        .each_ref()
        .map(|counter| counter.load(Ordering::Relaxed));
    TlsCounters {
        handshakes: values[0],
        ktls_tx_connections: values[1],
        ktls_rx_connections: values[2],
        encrypted_fallback_connections: values[3],
        tx_bytes: values[4],
        rx_bytes: values[5],
        sendfile_bytes: values[6],
        fallback_sendfile_bytes: values[7],
    }
}

#[repr(C)]
struct NativeResult {
    transferred: i64,
    status: i32,
    system_error: i32,
}

unsafe extern "C" {
    fn racer_tls_configure(ctx: *mut c_void, ktls: i32) -> i32;
    fn racer_tls_set_fd(ssl: *mut c_void, fd: i32, server: i32) -> i32;
    fn racer_tls_handshake(ssl: *mut c_void) -> NativeResult;
    fn racer_tls_read(ssl: *mut c_void, buf: *mut u8, len: usize) -> NativeResult;
    fn racer_tls_write(ssl: *mut c_void, buf: *const u8, len: usize) -> NativeResult;
    fn racer_tls_sendfile(ssl: *mut c_void, fd: i32, offset: i64, count: usize) -> NativeResult;
    fn racer_tls_shutdown(ssl: *mut c_void) -> NativeResult;
    fn racer_tls_offload(ssl: *mut c_void) -> i32;
    fn racer_tls_is_ca(cert: *mut c_void) -> i32;
}

#[derive(Eq, PartialEq)]
enum WriteSource {
    Bytes(usize),
    File {
        fd: RawFd,
        offset: u64,
        count: usize,
    },
}

struct PendingWrite {
    source: WriteSource,
    bytes: Vec<u8>,
}

pub struct TlsSession {
    // Field drop order is significant: SSL/BIO is freed before socket closes.
    ssl: Ssl,
    socket: OwnedFd,
    expected: ExpectedPeer,
    authenticated: bool,
    failed: bool,
    peer: Option<PeerIdentity>,
    local_expiry_unix: Option<u64>,
    peer_expiry_unix: Option<u64>,
    offload: Offload,
    counters: TlsCounters,
    pending: Option<PendingWrite>,
}

impl AsRawFd for TlsSession {
    fn as_raw_fd(&self) -> RawFd {
        self.socket.as_raw_fd()
    }
}

impl TlsSession {
    pub fn client(
        context: &TlsContext,
        socket: OwnedFd,
        expected: ExpectedPeer,
    ) -> io::Result<Self> {
        Self::new(context, socket, expected, false)
    }
    pub fn server(
        context: &TlsContext,
        socket: OwnedFd,
        expected: ExpectedPeer,
    ) -> io::Result<Self> {
        Self::new(context, socket, expected, true)
    }

    fn new(
        context: &TlsContext,
        socket: OwnedFd,
        expected: ExpectedPeer,
        server: bool,
    ) -> io::Result<Self> {
        if server && !context.has_identity {
            return Err(invalid("TLS server requires a local identity"));
        }
        let mut ssl = Ssl::new(&context.context).map_err(ssl_error)?;
        match &expected {
            ExpectedPeer::Identity(identity) => {
                PeerIdentity::new(&identity.universe, &identity.node, &identity.pod_uid)?;
            }
            ExpectedPeer::Universe(universe) => {
                if !is_id(universe) {
                    return Err(invalid("invalid expected universe"));
                }
            }
            ExpectedPeer::ControlPlane { dns_name } => {
                if server || dns_name.is_empty() || dns_name.contains('\0') {
                    return Err(invalid("invalid control-plane DNS expectation"));
                }
                ssl.set_hostname(dns_name).map_err(ssl_error)?;
                ssl.param_mut().set_hostflags(
                    X509CheckFlags::NEVER_CHECK_SUBJECT | X509CheckFlags::NO_WILDCARDS,
                );
                ssl.param_mut().set_host(dns_name).map_err(ssl_error)?;
            }
        }
        if unsafe { racer_tls_set_fd(ssl.as_ptr().cast(), socket.as_raw_fd(), i32::from(server)) }
            != 1
        {
            return Err(ssl_error(openssl::error::ErrorStack::get()));
        }
        Ok(Self {
            ssl,
            socket,
            expected,
            authenticated: false,
            failed: false,
            peer: None,
            local_expiry_unix: context.local_expiry_unix,
            peer_expiry_unix: None,
            offload: Offload::default(),
            counters: TlsCounters::default(),
            pending: None,
        })
    }

    fn decode(&mut self, result: NativeResult) -> io::Result<TlsProgress<usize>> {
        match result.status {
            0 => Ok(TlsProgress::Complete(result.transferred as usize)),
            1 => Ok(TlsProgress::WantRead),
            2 => Ok(TlsProgress::WantWrite),
            3 => Ok(TlsProgress::Eof),
            _ => {
                self.failed = true;
                let stack = openssl::error::ErrorStack::get();
                if result.status == 5 {
                    Err(io::Error::new(io::ErrorKind::UnexpectedEof, stack))
                } else if result.system_error != 0 {
                    Err(io::Error::from_raw_os_error(result.system_error))
                } else if stack.errors().is_empty() {
                    Err(io::Error::new(
                        io::ErrorKind::UnexpectedEof,
                        "TLS socket closed without close_notify",
                    ))
                } else {
                    Err(ssl_error(stack))
                }
            }
        }
    }

    pub fn handshake(&mut self) -> io::Result<TlsProgress<()>> {
        if self.failed {
            return Err(invalid("TLS session has failed"));
        }
        if self.authenticated {
            return Ok(TlsProgress::Complete(()));
        }
        let result = unsafe { racer_tls_handshake(self.ssl.as_ptr().cast()) };
        match self.decode(result)? {
            TlsProgress::Complete(_) => {
                // Mark failed before checking identity, so even a caller ignoring
                // the authentication error cannot read or write application data.
                self.failed = true;
                if self.ssl.verify_result() != openssl::x509::X509VerifyResult::OK
                    || self.ssl.session_reused()
                {
                    return Err(invalid("TLS peer verification or full handshake failed"));
                }
                let cert = self
                    .ssl
                    .peer_certificate()
                    .ok_or_else(|| invalid("TLS peer did not provide a certificate"))?;
                if unsafe { racer_tls_is_ca(cert.as_ptr().cast()) } != 0 {
                    return Err(invalid("TLS peer identity is a CA"));
                }
                let uri = uri_san(&cert)?;
                match &self.expected {
                    ExpectedPeer::ControlPlane { .. } => {
                        if uri != CONTROL_PLANE_URI {
                            return Err(invalid("control-plane URI SAN mismatch"));
                        }
                    }
                    ExpectedPeer::Identity(expected) => {
                        let actual = PeerIdentity::parse(&uri)?;
                        if &actual != expected {
                            return Err(invalid("TLS peer identity mismatch"));
                        }
                        self.peer = Some(actual);
                    }
                    ExpectedPeer::Universe(expected) => {
                        let actual = PeerIdentity::parse(&uri)?;
                        if &actual.universe != expected {
                            return Err(invalid("TLS peer universe mismatch"));
                        }
                        self.peer = Some(actual);
                    }
                }
                let bits = unsafe { racer_tls_offload(self.ssl.as_ptr().cast()) };
                self.peer_expiry_unix = Some(expiry(&cert)?);
                self.offload = Offload {
                    tx: bits & 1 != 0,
                    rx: bits & 2 != 0,
                };
                self.counters.handshakes = 1;
                self.counters.ktls_tx_connections = u64::from(self.offload.tx);
                self.counters.ktls_rx_connections = u64::from(self.offload.rx);
                self.counters.encrypted_fallback_connections =
                    u64::from(!self.offload.tx || !self.offload.rx);
                for (index, value) in [
                    1,
                    self.counters.ktls_tx_connections,
                    self.counters.ktls_rx_connections,
                    self.counters.encrypted_fallback_connections,
                ]
                .into_iter()
                .enumerate()
                {
                    COUNTERS[index].fetch_add(value, Ordering::Relaxed);
                }
                self.authenticated = true;
                self.failed = false;
                Ok(TlsProgress::Complete(()))
            }
            TlsProgress::WantRead => Ok(TlsProgress::WantRead),
            TlsProgress::WantWrite => Ok(TlsProgress::WantWrite),
            TlsProgress::Eof => {
                self.failed = true;
                Ok(TlsProgress::Eof)
            }
        }
    }

    fn ready(&self) -> io::Result<()> {
        if !self.authenticated || self.failed {
            Err(invalid("TLS session is not authenticated"))
        } else {
            Ok(())
        }
    }

    pub fn peer_identity(&self) -> Option<&PeerIdentity> {
        self.peer.as_ref()
    }

    /// Expiry of the local leaf captured when this session was created. Bootstrap
    /// enrollment sessions have no local certificate and return None.
    pub fn local_expiry_unix(&self) -> Option<u64> {
        self.local_expiry_unix
    }

    /// Expiry of the authenticated peer leaf, unavailable before authentication.
    pub fn peer_expiry_unix(&self) -> Option<u64> {
        self.peer_expiry_unix
    }

    /// Exclusive Unix-second deadline for admitting new requests. None means the
    /// session is not authenticated or has failed. For enrollment, only the peer
    /// leaf bounds validity. Context rotation cannot extend this session's limit.
    /// Existing transfers may finish: read/write/sendfile do not enforce expiry.
    pub fn valid_until(&self) -> Option<u64> {
        if !self.authenticated || self.failed {
            return None;
        }
        self.peer_expiry_unix
            .map(|peer| self.local_expiry_unix.map_or(peer, |local| local.min(peer)))
    }

    /// Admission only; callers separately authorize against current topology.
    pub fn admits_new_request(&self, now_unix: u64) -> bool {
        self.valid_until().is_some_and(|expiry| now_unix < expiry)
    }

    /// Account for file bytes read asynchronously by the transport and then
    /// successfully encrypted with write(). Do not count WANT or failed writes.
    pub fn record_fallback_sendfile_bytes(&mut self, bytes: usize) {
        self.counters.fallback_sendfile_bytes += bytes as u64;
        COUNTERS[7].fetch_add(bytes as u64, Ordering::Relaxed);
    }
    pub fn offload(&self) -> Offload {
        self.offload
    }
    pub fn counters(&self) -> TlsCounters {
        self.counters
    }

    pub fn read(&mut self, bytes: &mut [u8]) -> io::Result<TlsProgress<usize>> {
        self.ready()?;
        if bytes.is_empty() {
            return Ok(TlsProgress::Complete(0));
        }
        let result =
            unsafe { racer_tls_read(self.ssl.as_ptr().cast(), bytes.as_mut_ptr(), bytes.len()) };
        let progress = self.decode(result)?;
        if let TlsProgress::Complete(n) = progress {
            self.counters.rx_bytes += n as u64;
            COUNTERS[5].fetch_add(n as u64, Ordering::Relaxed);
        }
        Ok(progress)
    }

    /// Accepts at most 64 KiB per call. On WANT the session retains stable bytes;
    /// retries must supply the same bytes and original length.
    pub fn write(&mut self, bytes: &[u8]) -> io::Result<TlsProgress<usize>> {
        self.ready()?;
        let source = WriteSource::Bytes(bytes.len());
        let chunk = &bytes[..bytes.len().min(WRITE_CHUNK)];
        if let Some(pending) = &self.pending {
            if pending.source != source || pending.bytes != chunk {
                return Err(invalid("TLS write retry changed pending data"));
            }
        } else {
            if bytes.is_empty() {
                return Ok(TlsProgress::Complete(0));
            }
            self.pending = Some(PendingWrite {
                source,
                bytes: chunk.to_vec(),
            });
        }
        self.write_pending()
    }

    fn write_pending(&mut self) -> io::Result<TlsProgress<usize>> {
        let pending = self.pending.as_ref().unwrap();
        let result = unsafe {
            racer_tls_write(
                self.ssl.as_ptr().cast(),
                pending.bytes.as_ptr(),
                pending.bytes.len(),
            )
        };
        let progress = self.decode(result)?;
        if let TlsProgress::Complete(n) = progress {
            self.pending = None;
            self.counters.tx_bytes += n as u64;
            COUNTERS[4].fetch_add(n as u64, Ordering::Relaxed);
        }
        Ok(progress)
    }

    /// Uses SSL_sendfile only when OpenSSL reports actual TX kTLS. Otherwise
    /// pread into a bounded retained buffer and encrypt with SSL_write_ex.
    pub fn sendfile(
        &mut self,
        file: BorrowedFd<'_>,
        offset: u64,
        count: usize,
    ) -> io::Result<TlsProgress<usize>> {
        self.ready()?;
        let offset_i64 =
            i64::try_from(offset).map_err(|_| invalid("TLS sendfile offset overflow"))?;
        let source = WriteSource::File {
            fd: file.as_raw_fd(),
            offset,
            count,
        };
        if let Some(pending) = &self.pending {
            if pending.source != source {
                return Err(invalid("TLS sendfile retry changed pending operation"));
            }
        } else if count == 0 {
            return Ok(TlsProgress::Complete(0));
        }
        if self.offload.tx {
            // Retain operation identity even though kTLS does not use a buffer.
            if self.pending.is_none() {
                self.pending = Some(PendingWrite {
                    source,
                    bytes: Vec::new(),
                });
            }
            let result = unsafe {
                racer_tls_sendfile(
                    self.ssl.as_ptr().cast(),
                    file.as_raw_fd(),
                    offset_i64,
                    count.min(i32::MAX as usize),
                )
            };
            let progress = self.decode(result)?;
            if let TlsProgress::Complete(n) = progress {
                self.pending = None;
                self.counters.tx_bytes += n as u64;
                self.counters.sendfile_bytes += n as u64;
                COUNTERS[4].fetch_add(n as u64, Ordering::Relaxed);
                COUNTERS[6].fetch_add(n as u64, Ordering::Relaxed);
            }
            return Ok(progress);
        }
        if self.pending.is_none() {
            let mut bytes = vec![0; count.min(WRITE_CHUNK)];
            let n = loop {
                let rc = unsafe {
                    libc::pread(
                        file.as_raw_fd(),
                        bytes.as_mut_ptr().cast(),
                        bytes.len(),
                        offset_i64,
                    )
                };
                if rc >= 0 {
                    break rc as usize;
                }
                let error = io::Error::last_os_error();
                if error.kind() != io::ErrorKind::Interrupted {
                    return Err(error);
                }
            };
            if n == 0 {
                return Ok(TlsProgress::Complete(0));
            }
            bytes.truncate(n);
            self.pending = Some(PendingWrite { source, bytes });
        }
        let progress = self.write_pending()?;
        if let TlsProgress::Complete(n) = progress {
            self.record_fallback_sendfile_bytes(n);
        }
        Ok(progress)
    }

    pub fn shutdown(&mut self) -> io::Result<TlsProgress<()>> {
        self.ready()?;
        if self.pending.is_some() {
            return Err(invalid("TLS shutdown with a pending write"));
        }
        let result = unsafe { racer_tls_shutdown(self.ssl.as_ptr().cast()) };
        match self.decode(result)? {
            TlsProgress::Complete(_) => Ok(TlsProgress::Complete(())),
            TlsProgress::WantRead => Ok(TlsProgress::WantRead),
            TlsProgress::WantWrite => Ok(TlsProgress::WantWrite),
            TlsProgress::Eof => Ok(TlsProgress::Eof),
        }
    }
}

#[cfg(test)]
#[path = "../tests/security/tls.rs"]
pub(crate) mod tests;
