//! Durable node-private enrollment, independent of transport and hardware inventory.
//! The caller persists NIC reservations before preparing a request, supplies current
//! NICs and shares, and exclusively owns this identity directory. Attempt fencing
//! must remain bound to the same serving reactor for the lifetime of this owner.
//!
//! Prepare persists a retry-stable key before exposing its CSR. Accept validates
//! the response before durable replacement, and recovery revalidates trust before
//! returning an identity. Cancellation and abandoned writes are fenced through
//! the caller's host; transport retries and inventory remain application policy.

use crate::identity;
use base64::{Engine, engine::general_purpose::STANDARD};
use racer_control_wire::{
    self as wire, ClusterId, EnrollmentId, EnrollmentRequest, EnrollmentResponse, NodeId,
    RailMapping,
};
use serde::{Deserialize, Serialize};
use std::{
    cell::{Cell, RefCell},
    num::NonZeroU32,
    path::PathBuf,
    sync::Arc,
    time::{Duration, SystemTime, UNIX_EPOCH},
};
use uring_runtime::{
    Operation, Scope,
    drivers::Busy,
    reactor::filesystem::{
        operations::ReplacementError,
        secure::{self, Attempts, Host},
    },
};
use zeroize::{Zeroize, Zeroizing};

/// Enrollment validation failures, separate from caller-owned filesystem errors.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Error {
    /// Trust replacement overlaps identity acceptance or recovery. Retry when idle.
    Busy,

    /// Identity correlation, certificate, token, or key pairing failed.
    Unauthorized,

    /// Persisted JSON or private key material is corrupt.
    CorruptRecord,

    /// The configured cluster is not a canonical UUID.
    InvalidConfiguration,

    /// Entropy, serialization, or local key generation failed.
    Io,

    /// Preserve the wire codec's exact failure classification.
    Wire(wire::Error),
}

impl From<wire::Error> for Error {
    /// Retain the wire failure category without including rejected record bytes.
    fn from(error: wire::Error) -> Self {
        Self::Wire(error)
    }
}

/// Node-bound key and certificate persistence on a caller-owned filesystem host.
pub struct Enrollment<H: Host> {
    cluster: ClusterId,

    token_path: PathBuf,

    identity_directory: PathBuf,

    roots: RefCell<Vec<Vec<u8>>>,

    trust_busy: Cell<bool>,

    host: H,

    attempts: Attempts<H::Scope>,
}

/// A validated local key. Secret storage is private and zeroizes on final drop.
#[derive(Clone)]
pub struct LocalSigningIdentity {
    cluster: ClusterId,

    node: NodeId,

    enrollment: EnrollmentId,

    private_material: Zeroizing<Vec<u8>>,

    certificate_chain: Vec<Vec<u8>>,

    block_devices: Option<String>,

    not_before: u64,

    not_after: u64,
}

/// Retry-stable persisted CSR and private key, never included in diagnostics.
#[derive(Serialize, Deserialize)]
struct PendingIdentity {
    cluster: String,

    enrollment: String,

    private_key: EncodedPrivateKey,

    csr: String,
}

/// Own secret text before serde has constructed the surrounding record.
struct EncodedPrivateKey(Zeroizing<String>);

impl Serialize for EncodedPrivateKey {
    /// Preserve the persisted base64 string schema.
    fn serialize<S: serde::Serializer>(&self, serializer: S) -> Result<S::Ok, S::Error> {
        serializer.serialize_str(&self.0)
    }
}

impl<'de> Deserialize<'de> for EncodedPrivateKey {
    /// Protect a decoded field immediately, even if a later field is missing.
    fn deserialize<D: serde::Deserializer<'de>>(deserializer: D) -> Result<Self, D::Error> {
        String::deserialize(deserializer).map(|value| Self(Zeroizing::new(value)))
    }
}

/// Clear all JSON strings, including values not consumed by a failing DTO decode.
struct PrivateJson(serde_json::Value);

impl Drop for PrivateJson {
    /// Erase temporary copies while their allocations are still owned.
    fn drop(&mut self) {
        /// Visit every string in a private record's JSON tree.
        fn clear(value: &mut serde_json::Value) {
            match value {
                serde_json::Value::String(text) => text.zeroize(),
                serde_json::Value::Array(values) => values.iter_mut().for_each(clear),
                serde_json::Value::Object(values) => values.values_mut().for_each(clear),
                _ => (),
            }
        }
        clear(&mut self.0);
    }
}

/// Exact pending request paired with the accepted wire response for recovery.
#[derive(Serialize, Deserialize)]
struct PersistedIdentity {
    pending: PendingIdentity,

    response: String,
}

impl PersistedIdentity {
    /// Reject duplicate fields and oversized records before deserializing.
    fn decode(bytes: &[u8]) -> Result<Self, Error> {
        let scratch = PrivateJson(wire::strict_json(bytes, wire::MAX_ENROLLMENT_BYTES * 3)?);
        Self::deserialize(&scratch.0).map_err(|_| Error::CorruptRecord)
    }

    /// Decode the saved response through the same wire validation as live traffic.
    fn response(&self) -> Result<EnrollmentResponse, Error> {
        Ok(wire::decode_enrollment_response(
            &STANDARD
                .decode(&self.response)
                .map_err(|_| Error::CorruptRecord)?,
        )?)
    }

    /// Keep serialized private material in zeroizing storage until submission.
    fn encode(&self) -> Result<Zeroizing<Vec<u8>>, Error> {
        encode_private(self, wire::MAX_ENROLLMENT_BYTES * 3)
    }
}

impl<H: Host> Enrollment<H> {
    /// Configure a namespace owner without performing I/O or generating keys.
    pub fn new(
        cluster: ClusterId,
        token_path: PathBuf,
        identity_directory: PathBuf,
        host: H,
    ) -> Self {
        Self {
            cluster,
            token_path,
            identity_directory,
            roots: RefCell::new(Vec::new()),
            trust_busy: Cell::new(false),
            host,
            attempts: Attempts::default(),
        }
    }

    /// Return the configured cluster binding.
    pub fn cluster(&self) -> &ClusterId {
        &self.cluster
    }

    /// Validate all roots before atomically replacing the enrollment trust snapshot.
    /// Return Busy while acceptance or recovery holds trust across filesystem awaits.
    ///
    /// The caller must authenticate the source and authorize trust-root replacement.
    /// Parsing and validity checks do not establish that authority or require new
    /// roots to chain to existing roots.
    pub fn set_peer_trust_roots(&self, roots: Vec<Vec<u8>>) -> Result<(), Error> {
        let _guard = Busy::try_enter(&self.trust_busy).map_err(|_| Error::Busy)?;
        identity::root_store(&roots).map_err(|_| Error::Unauthorized)?;
        *self.roots.borrow_mut() = roots;
        Ok(())
    }

    /// Generate a local Ed25519 key and a retry-stable random enrollment UUID.
    fn generate(&self) -> Result<PendingIdentity, Error> {
        if !wire::valid_uuid(&self.cluster.0) {
            return Err(Error::InvalidConfiguration);
        }
        let key = identity::PendingIdentity::generate().map_err(|_| Error::Io)?;
        let private = key.export_pkcs8_for_persistence().map_err(|_| Error::Io)?;
        let signing = key.rcgen_key_pair().map_err(|_| Error::Io)?;
        let mut params =
            rcgen::CertificateParams::new(Vec::<String>::new()).map_err(|_| Error::Io)?;
        params.distinguished_name = rcgen::DistinguishedName::new();
        let csr = params.serialize_request(&signing).map_err(|_| Error::Io)?;
        let mut id = [0; 16];
        uring_runtime::environment::fill_random(&mut id).map_err(|_| Error::Io)?;
        id[6] = (id[6] & 0x0f) | 0x40;
        id[8] = (id[8] & 0x3f) | 0x80;
        let h = format!("{:032x}", u128::from_be_bytes(id));
        Ok(PendingIdentity {
            cluster: self.cluster.0.clone(),
            enrollment: format!(
                "{}-{}-{}-{}-{}",
                &h[..8],
                &h[8..12],
                &h[12..16],
                &h[16..20],
                &h[20..]
            ),
            private_key: EncodedPrivateKey(Zeroizing::new(STANDARD.encode(&*private))),
            csr: STANDARD.encode(csr.der()),
        })
    }

    /// Bind caller inventory to a checked, retry-stable CSR without owning hardware.
    fn request(
        &self,
        p: &PendingIdentity,
        rdma_nics: Vec<RailMapping>,
        shares: NonZeroU32,
    ) -> Result<EnrollmentRequest, Error> {
        if p.cluster != self.cluster.0 {
            return Err(Error::Unauthorized);
        }
        let request = EnrollmentRequest {
            rdma_nics,
            shares: shares.get(),
            schema_version: wire::SCHEMA_VERSION,
            cluster: self.cluster.clone(),
            enrollment: EnrollmentId(p.enrollment.clone()),
            csr_der: STANDARD.decode(&p.csr).map_err(|_| Error::CorruptRecord)?,
        };
        wire::encode_enrollment_request(&request)?;
        check_pending_key(p, &request.csr_der)?;
        Ok(request)
    }

    /// Validate request correlation and certificate policy before any publication.
    fn accept_pending(
        &self,
        bytes: &[u8],
        response: &EnrollmentResponse,
    ) -> Result<(LocalSigningIdentity, Zeroizing<Vec<u8>>), Error> {
        let pending = decode_pending(bytes)?;
        self.request(
            &pending,
            Vec::new(),
            NonZeroU32::new(wire::DEFAULT_SHARES).unwrap(),
        )?;
        let identity = self.validate(&pending, response)?;
        let persisted = PersistedIdentity {
            pending,
            response: STANDARD.encode(wire::encode_enrollment_response(response)?),
        };
        Ok((identity, persisted.encode()?))
    }

    /// Validate the exact enrollment certificate contract, including leaf lifetime.
    fn validate(
        &self,
        p: &PendingIdentity,
        r: &EnrollmentResponse,
    ) -> Result<LocalSigningIdentity, Error> {
        wire::encode_enrollment_response(r)?;
        if r.cluster != self.cluster
            || p.cluster != self.cluster.0
            || r.enrollment.0 != p.enrollment
        {
            return Err(Error::Unauthorized);
        }
        let public = identity::verify_chain(
            &self.roots.borrow(),
            &r.certificate_chain,
            &self.cluster,
            &r.node,
        )
        .map_err(|_| Error::Unauthorized)?;
        let (_, cert) = x509_parser::parse_x509_certificate(&r.certificate_chain[0])
            .map_err(|_| Error::Unauthorized)?;
        let san = cert
            .subject_alternative_name()
            .map_err(|_| Error::Unauthorized)?
            .ok_or(Error::Unauthorized)?;
        // Activation permits other non-URI names. Issuance admits exactly one SAN.
        if san.value.general_names.len() != 1 {
            return Err(Error::Unauthorized);
        }
        let private_material = decode_private_key(&p.private_key)?;
        let key = crate::SigningKey::from_pkcs8_der(&private_material)
            .map_err(|_| Error::CorruptRecord)?;
        if public.key != key.verifying_key() {
            return Err(Error::Unauthorized);
        }
        let not_before = u64::try_from(cert.validity().not_before.timestamp())
            .map_err(|_| Error::Unauthorized)?;
        let not_after = u64::try_from(cert.validity().not_after.timestamp())
            .map_err(|_| Error::Unauthorized)?;
        if not_after <= not_before
            || not_after - not_before > wire::CERTIFICATE_LIFETIME.as_secs() + 300
        {
            return Err(Error::Unauthorized);
        }
        Ok(LocalSigningIdentity {
            cluster: self.cluster.clone(),
            node: r.node.clone(),
            enrollment: r.enrollment.clone(),
            private_material,
            certificate_chain: r.certificate_chain.clone(),
            block_devices: r
                .block_devices
                .clone()
                .filter(|selector| !selector.is_empty()),
            not_before: public.valid_from,
            not_after: public.expires,
        })
    }
}

impl<H: Host> Enrollment<H>
where
    <H::Scope as Scope>::Error: From<Error>
        + From<secure::AccessError>
        + From<ReplacementError<<H::Scope as Scope>::Error>>,
{
    /// Persist the private key before returning a request. The caller must durably
    /// persist its NIC reservations before calling this method, including retries.
    pub fn prepare<'a>(
        &'a self,
        nics: Vec<RailMapping>,
        shares: NonZeroU32,
        parent: &'a H::Scope,
    ) -> Operation<'a, EnrollmentRequest, <H::Scope as Scope>::Error> {
        Box::pin(async move {
            let (_guard, scope) = self.attempts.begin(&self.host, parent).await?;
            let r = self.host.reactor();
            let dir = secure::directory(r, &self.identity_directory, true, true, &scope).await?;
            match secure::read_at(
                r,
                &dir,
                "pending.json",
                wire::MAX_ENROLLMENT_BYTES,
                true,
                &scope,
            )
            .await
            {
                Ok(bytes) => {
                    r.file_sync(dir.clone(), &scope).await?;
                    return self
                        .request(&decode_pending(&bytes)?, nics, shares)
                        .map_err(Into::into);
                }
                Err(e) if H::is_missing(e) => (),
                Err(e) => return Err(e),
            }
            let pending = self.generate()?;
            let encoded = encode_private(&pending, wire::MAX_ENROLLMENT_BYTES)?;
            secure::atomic_write(r, &dir, "pending.json", &encoded, &scope).await?;
            self.request(&pending, nics, shares).map_err(Into::into)
        })
    }

    /// Read the current projected token without caching rotations or rejecting symlinks.
    pub fn read_token<'a>(
        &'a self,
        scope: &'a H::Scope,
    ) -> Operation<'a, Zeroizing<String>, <H::Scope as Scope>::Error> {
        Box::pin(async move {
            let bytes = secure::read_path(
                self.host.reactor(),
                &self.token_path,
                wire::MAX_ENROLLMENT_BYTES,
                scope,
            )
            .await?;
            token(&bytes).map_err(Into::into)
        })
    }

    /// Validate, durably publish identity, then durably remove the pending request.
    /// Publication errors are converted by the caller without discarding their phase.
    ///
    /// The caller must authenticate and authorize the source of the whole response.
    /// Structural, certificate, correlation, and key-pair checks do not authenticate
    /// the whole response: the certificate does not cover `schema_version`,
    /// `enrollment`, or `block_devices`. The selector is copied, not authorized here.
    pub fn accept_response<'a>(
        &'a self,
        response: EnrollmentResponse,
        parent: &'a H::Scope,
    ) -> Operation<'a, LocalSigningIdentity, <H::Scope as Scope>::Error> {
        Box::pin(async move {
            let (_guard, scope) = self.attempts.begin(&self.host, parent).await?;
            let _trust = Busy::try_enter(&self.trust_busy).map_err(|_| Error::Busy)?;
            let r = self.host.reactor();
            let dir = secure::directory(r, &self.identity_directory, false, true, &scope).await?;
            match secure::read_at(
                r,
                &dir,
                "identity.json",
                wire::MAX_ENROLLMENT_BYTES * 3,
                true,
                &scope,
            )
            .await
            {
                Ok(bytes) => {
                    let existing = PersistedIdentity::decode(&bytes)?.response()?;
                    if existing.cluster != response.cluster || existing.node != response.node {
                        return Err(Error::Unauthorized.into());
                    }
                }
                Err(e) if H::is_missing(e) => (),
                Err(e) => return Err(e),
            }
            let bytes = secure::read_at(
                r,
                &dir,
                "pending.json",
                wire::MAX_ENROLLMENT_BYTES,
                true,
                &scope,
            )
            .await?;
            let (identity, bytes) = self.accept_pending(&bytes, &response)?;
            secure::atomic_write(r, &dir, "identity.json", &bytes, &scope).await?;
            secure::remove(r, &dir, "pending.json", &scope).await?;
            Ok(identity)
        })
    }

    /// Recover and revalidate a published identity, fencing uncertain publication
    /// and removing only a pending request already represented by that identity.
    pub fn load_identity<'a>(
        &'a self,
        parent: &'a H::Scope,
    ) -> Operation<'a, Option<LocalSigningIdentity>, <H::Scope as Scope>::Error> {
        Box::pin(async move {
            let (_guard, scope) = self.attempts.begin(&self.host, parent).await?;
            let _trust = Busy::try_enter(&self.trust_busy).map_err(|_| Error::Busy)?;
            let r = self.host.reactor();
            let dir =
                match secure::directory(r, &self.identity_directory, false, true, &scope).await {
                    Ok(dir) => dir,
                    Err(e) if H::is_missing(e) => return Ok(None),
                    Err(e) => return Err(e),
                };
            let bytes = match secure::read_at(
                r,
                &dir,
                "identity.json",
                wire::MAX_ENROLLMENT_BYTES * 3,
                true,
                &scope,
            )
            .await
            {
                Ok(bytes) => bytes,
                Err(e) if H::is_missing(e) => return Ok(None),
                Err(e) => return Err(e),
            };
            let persisted = PersistedIdentity::decode(&bytes)?;
            let response = persisted.response()?;
            match self.validate(&persisted.pending, &response) {
                Ok(identity) => {
                    r.file_sync(dir.clone(), &scope).await?;
                    match secure::read_at(
                        r,
                        &dir,
                        "pending.json",
                        wire::MAX_ENROLLMENT_BYTES,
                        true,
                        &scope,
                    )
                    .await
                    {
                        Ok(bytes)
                            if decode_pending(&bytes).is_ok_and(|pending| {
                                pending.enrollment == identity.enrollment.0
                            }) =>
                        {
                            secure::remove(r, &dir, "pending.json", &scope).await?
                        }
                        Ok(_) => (),
                        Err(e) if H::is_missing(e) => (),
                        Err(e) => return Err(e),
                    }
                    Ok(Some(identity))
                }
                Err(Error::Unauthorized) => Ok(None),
                Err(e) => Err(e.into()),
            }
        })
    }
}

impl LocalSigningIdentity {
    /// Borrow the accepted block-device selector, with empty selectors treated as absent.
    pub fn block_devices(&self) -> Option<&str> {
        self.block_devices.as_deref()
    }

    /// Return the authenticated node UID.
    pub fn node(&self) -> &NodeId {
        &self.node
    }

    /// Return the authenticated cluster UID.
    pub fn cluster(&self) -> &ClusterId {
        &self.cluster
    }

    /// Borrow the exact accepted leaf-first certificate chain for REST/TLS.
    pub fn certificate_chain(&self) -> &[Vec<u8>] {
        &self.certificate_chain
    }

    /// Explicitly borrow PKCS8 material for the REST client's TLS identity adapter.
    /// The borrower must not persist, log, or share this key through cluster Secrets.
    pub fn private_key_der(&self) -> &[u8] {
        &self.private_material
    }

    /// Revalidate this identity against the signing subsystem's current peer trust.
    pub fn signing_identity(
        &self,
        roots: &[Vec<u8>],
    ) -> identity::Result<Arc<identity::SigningIdentity>> {
        identity::SigningIdentity::from_pkcs8(
            self.cluster.clone(),
            self.node.clone(),
            &self.private_material,
            self.certificate_chain.clone(),
            roots,
        )
        .map(Arc::new)
    }

    /// Return the chain and trust roots' earliest expiry for REST lifetime checks.
    pub fn expires_at(&self) -> SystemTime {
        UNIX_EPOCH + Duration::from_secs(self.not_after)
    }

    /// Check the chain and trust roots' validity bounds against the scoped wall clock.
    pub fn valid_now(&self) -> bool {
        let now = uring_runtime::environment::wall_now();
        now >= UNIX_EPOCH + Duration::from_secs(self.not_before) && now < self.expires_at()
    }

    /// Renew after two thirds of the trusted lifetime, capped by wire policy.
    pub fn renewal_due(&self) -> bool {
        let lifetime = Duration::from_secs(self.not_after - self.not_before);
        uring_runtime::environment::wall_now()
            >= UNIX_EPOCH
                + Duration::from_secs(self.not_before)
                + (lifetime * 2 / 3).min(wire::RENEW_AFTER)
    }
}

/// Fixed-capacity output that cannot reallocate after receiving secret bytes.
struct PrivateOutput {
    bytes: Zeroizing<Vec<u8>>,

    limit: usize,
}

impl std::io::Write for PrivateOutput {
    /// Reject an oversized write before copying any bytes or growing the allocation.
    fn write(&mut self, bytes: &[u8]) -> std::io::Result<usize> {
        if bytes.len() > self.limit - self.bytes.len() {
            return Err(std::io::ErrorKind::FileTooLarge.into());
        }
        self.bytes.extend_from_slice(bytes);
        Ok(bytes.len())
    }

    /// Serialization writes directly to memory, so there is nothing to flush.
    fn flush(&mut self) -> std::io::Result<()> {
        Ok(())
    }
}

/// Serialize directly into wiping storage sized before any secrets are copied.
fn encode_private(value: &impl Serialize, limit: usize) -> Result<Zeroizing<Vec<u8>>, Error> {
    let mut output = PrivateOutput {
        bytes: Zeroizing::new(Vec::with_capacity(limit)),
        limit,
    };
    serde_json::to_writer(&mut output, value).map_err(|_| Error::Io)?;
    Ok(output.bytes)
}

/// Parse a projected bearer credential without retaining leading/trailing whitespace.
fn token(bytes: &[u8]) -> Result<Zeroizing<String>, Error> {
    let token = Zeroizing::new(
        std::str::from_utf8(bytes)
            .map_err(|_| Error::Unauthorized)?
            .trim()
            .to_owned(),
    );
    if token.is_empty()
        || !token
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || matches!(b, b'-' | b'_' | b'.'))
    {
        return Err(Error::Unauthorized);
    }
    Ok(token)
}

/// Decode bounded, duplicate-free pending JSON without changing its on-disk schema.
fn decode_pending(bytes: &[u8]) -> Result<PendingIdentity, Error> {
    let scratch = PrivateJson(wire::strict_json(bytes, wire::MAX_ENROLLMENT_BYTES)?);
    PendingIdentity::deserialize(&scratch.0).map_err(|_| Error::CorruptRecord)
}

/// Own bounded wiping output before base64 can write even a partial private key.
fn decode_private_key(encoded: &EncodedPrivateKey) -> Result<Zeroizing<Vec<u8>>, Error> {
    if encoded.0.len() > wire::MAX_ENROLLMENT_BYTES {
        return Err(Error::CorruptRecord);
    }
    // The encoded record bound makes this rounded decoded capacity nonoverflowing.
    let capacity = encoded.0.len().div_ceil(4) * 3;
    let mut secret = Zeroizing::new(vec![0; capacity]);
    let length = STANDARD
        .decode_slice(encoded.0.as_bytes(), secret.as_mut_slice())
        .map_err(|_| Error::CorruptRecord)?;
    secret.truncate(length);
    Ok(secret)
}

/// Reject a corrupt or mismatched pending key before submitting its CSR.
fn check_pending_key(pending: &PendingIdentity, csr: &[u8]) -> Result<(), Error> {
    use x509_parser::prelude::FromDer;
    let secret = decode_private_key(&pending.private_key)?;
    let key = crate::SigningKey::from_pkcs8_der(&secret).map_err(|_| Error::CorruptRecord)?;
    let (_, csr) = x509_parser::certification_request::X509CertificationRequest::from_der(csr)
        .map_err(|_| Error::CorruptRecord)?;
    if csr
        .certification_request_info
        .subject_pki
        .subject_public_key
        .data
        .as_ref()
        != key.verifying_key().as_bytes()
    {
        return Err(Error::Unauthorized);
    }
    csr.verify_signature().map_err(|_| Error::Unauthorized)
}

#[cfg(test)]
mod tests {
    //! Real enrollment state transitions over deterministic reactor completions.
    use super::*;
    use std::{
        cell::Cell,
        path::Path,
        rc::Rc,
        task::{Context, Poll},
        time::Instant,
    };
    use uring_runtime::{
        environment::Cancellation,
        reactor::{
            Reactor,
            simulation::{Fault, Simulation},
        },
    };

    std::thread_local! {
        /// Observe secret-field destruction without reading deallocated memory.
        static PRIVATE_KEY_DROPS: Cell<usize> = const { Cell::new(0) };
    }

    impl Drop for EncodedPrivateKey {
        /// The zeroizing field is erased after this test-only observer runs.
        fn drop(&mut self) {
            PRIVATE_KEY_DROPS.with(|drops| drops.set(drops.get() + 1));
        }
    }

    /// Partial pending and nested persisted records retain zeroizing field ownership.
    #[test]
    fn private_fields_drop_on_missing_and_late_invalid_record_fields() {
        for pending in [
            r#"{"cluster":"cluster","enrollment":"id","private_key":"secret"}"#,
            r#"{"cluster":"cluster","enrollment":"id","private_key":"secret","csr":false}"#,
        ] {
            // Direct serde parsing exercises input order; production also retains
            // a wiping JSON tree, whose sorted fields may fail before the key.
            PRIVATE_KEY_DROPS.with(|drops| drops.set(0));
            assert!(serde_json::from_str::<PendingIdentity>(pending).is_err());
            assert_eq!(PRIVATE_KEY_DROPS.with(Cell::get), 1);
            assert!(matches!(
                decode_pending(pending.as_bytes()),
                Err(Error::CorruptRecord)
            ));
            let persisted = format!(r#"{{"pending":{pending},"response":"response"}}"#);
            PRIVATE_KEY_DROPS.with(|drops| drops.set(0));
            assert!(serde_json::from_str::<PersistedIdentity>(&persisted).is_err());
            assert_eq!(PRIVATE_KEY_DROPS.with(Cell::get), 1);
            assert!(matches!(
                PersistedIdentity::decode(persisted.as_bytes()),
                Err(Error::CorruptRecord)
            ));
        }
        for tail in ["", r#", "response":false"#] {
            let persisted = format!(
                r#"{{"pending":{{"cluster":"cluster","enrollment":"id","private_key":"secret","csr":"csr"}}{tail}}}"#
            );
            PRIVATE_KEY_DROPS.with(|drops| drops.set(0));
            assert!(matches!(
                PersistedIdentity::decode(persisted.as_bytes()),
                Err(Error::CorruptRecord)
            ));
            assert_eq!(PRIVATE_KEY_DROPS.with(Cell::get), 1);
        }
    }

    /// Fixed storage bounds both record families, including late serializer errors.
    #[test]
    fn private_serialization_is_bounded_non_reallocating_and_round_trips() {
        use std::io::Write;
        let pending =
            r#"{"cluster":"cluster","enrollment":"id","private_key":"secret","csr":"csr"}"#;
        let record = decode_pending(pending.as_bytes()).unwrap();
        let encoded = encode_private(&record, pending.len()).unwrap();
        assert_eq!(&*encoded, pending.as_bytes());
        assert_eq!(
            decode_pending(&encoded).unwrap().private_key.0.as_str(),
            "secret"
        );
        assert!(matches!(
            encode_private(&record, pending.len() - 1),
            Err(Error::Io)
        ));
        let persisted = PersistedIdentity {
            pending: record,
            response: "response".into(),
        };
        let encoded = persisted.encode().unwrap();
        let decoded = PersistedIdentity::decode(&encoded).unwrap();
        assert_eq!(decoded.pending.private_key.0.as_str(), "secret");
        assert_eq!(decoded.response, "response");
        let oversized = PersistedIdentity {
            pending: decode_pending(pending.as_bytes()).unwrap(),
            response: "x".repeat(wire::MAX_ENROLLMENT_BYTES * 3),
        };
        assert!(matches!(oversized.encode(), Err(Error::Io)));
        let mut oversized_pending = decode_pending(pending.as_bytes()).unwrap();
        oversized_pending.csr = "x".repeat(wire::MAX_ENROLLMENT_BYTES);
        assert!(matches!(
            encode_private(&oversized_pending, wire::MAX_ENROLLMENT_BYTES),
            Err(Error::Io)
        ));

        let mut output = PrivateOutput {
            bytes: Zeroizing::new(Vec::with_capacity(6)),
            limit: 6,
        };
        let pointer = output.bytes.as_ptr();
        let capacity = output.bytes.capacity();
        for part in [b"sec".as_slice(), b"ret"] {
            output.write_all(part).unwrap();
            assert_eq!(output.bytes.as_ptr(), pointer);
            assert_eq!(output.bytes.capacity(), capacity);
        }
        assert_eq!(
            output.write(b"! ").unwrap_err().kind(),
            std::io::ErrorKind::FileTooLarge
        );
        assert_eq!(&*output.bytes, b"secret");
        assert_eq!(output.bytes.as_ptr(), pointer);
        assert_eq!(output.bytes.capacity(), capacity);

        /// Fail after the serializer has already emitted a secret-bearing field.
        struct FailsLate;
        impl Serialize for FailsLate {
            /// Exercise cleanup after a non-capacity serialization failure.
            fn serialize<S: serde::Serializer>(&self, serializer: S) -> Result<S::Ok, S::Error> {
                use serde::ser::SerializeStruct;
                let mut record = serializer.serialize_struct("record", 2)?;
                record.serialize_field("private_key", "secret")?;
                Err(serde::ser::Error::custom("late failure"))
            }
        }
        assert!(matches!(encode_private(&FailsLate, 128), Err(Error::Io)));
    }

    /// Flat cause and publication phase keep test adapter errors lossless.
    #[derive(Clone, Copy, Debug, Eq, PartialEq)]
    struct Failure {
        cause: Cause,

        phase: u8,
    }

    /// Independent component errors crossing the test host boundary.
    #[derive(Clone, Copy, Debug, Eq, PartialEq)]
    enum Cause {
        Runtime(uring_runtime::Error),

        Enrollment(Error),

        Access(secure::AccessError),
    }

    impl From<uring_runtime::Error> for Failure {
        /// Retain the runtime cause before any replacement phase is attached.
        fn from(e: uring_runtime::Error) -> Self {
            Self {
                cause: Cause::Runtime(e),
                phase: 0,
            }
        }
    }

    impl From<Error> for Failure {
        /// Retain enrollment validation failures independently of filesystem causes.
        fn from(e: Error) -> Self {
            Self {
                cause: Cause::Enrollment(e),
                phase: 0,
            }
        }
    }

    impl From<secure::AccessError> for Failure {
        /// Preserve secure access policy failures without flattening their category.
        fn from(e: secure::AccessError) -> Self {
            Self {
                cause: Cause::Access(e),
                phase: 0,
            }
        }
    }

    impl From<ReplacementError<Failure>> for Failure {
        /// Preserve whether replacement failed before, during, or after publication.
        fn from(e: ReplacementError<Failure>) -> Self {
            match e {
                ReplacementError::BeforeRename(e) => e,
                ReplacementError::RenameUncertain(e) => Self { phase: 1, ..e },
                ReplacementError::Published(e) => Self { phase: 2, ..e },
            }
        }
    }

    /// Fresh IDs retain parent cancellation but identify only one attempt's I/O.
    #[derive(Clone)]
    struct Request {
        id: u64,

        cancellation: Cancellation,
    }

    impl Scope for Request {
        type Error = Failure;

        /// Stop cancelled requests before admitting more simulated I/O.
        fn check(&self) -> Result<(), Failure> {
            if self.cancellation.is_cancelled() {
                Err(uring_runtime::Error::Cancelled.into())
            } else {
                Ok(())
            }
        }

        /// Share parent cancellation with the fresh attempt scope.
        fn cancellation(&self) -> Option<&Cancellation> {
            Some(&self.cancellation)
        }
    }

    /// Deterministic worker hosting no dataplane state or hardware inventory.
    struct Files {
        reactor: Reactor<Request, ()>,

        next: Cell<u64>,
    }

    impl Host for Files {
        type Scope = Request;

        type Budget = ();

        /// Keep every attempt bound to this serving reactor.
        fn reactor(&self) -> &Reactor<Request, ()> {
            &self.reactor
        }

        /// Assign a distinct I/O identity while retaining parent cancellation.
        fn fresh_scope(&self, parent: &Request) -> Result<Request, Failure> {
            self.next.set(self.next.get() + 1);
            Ok(Request {
                id: self.next.get(),
                ..parent.clone()
            })
        }

        /// Drain only the abandoned attempt before a successor can access its files.
        fn fence<'a>(&'a self, previous: &'a Request) -> Operation<'a, (), Failure> {
            self.reactor
                .fence_matching(move |scope| scope.id == previous.id)
        }

        /// Distinguish absent records from other filesystem and policy failures.
        fn is_missing(error: Failure) -> bool {
            error == uring_runtime::Error::NotFound.into()
        }
    }

    /// Canonical authority used for enrollment correlation in simulation.
    const CLUSTER: &str = "11111111-1111-4111-8111-111111111111";

    /// Node identity assigned by the simulated certificate issuer.
    const NODE: &str = "22222222-2222-4222-8222-222222222222";

    /// Construct one namespace owner on the active simulation backend.
    fn fixture() -> (Rc<Files>, Enrollment<Rc<Files>>, Request) {
        let files = Rc::new(Files {
            reactor: Reactor::new(64, ()),
            next: Cell::new(0),
        });
        let enrollment = Enrollment::new(
            ClusterId(CLUSTER.into()),
            "/token".into(),
            "/private".into(),
            files.clone(),
        );
        (
            files,
            enrollment,
            Request {
                id: 0,
                cancellation: Cancellation::new().unwrap(),
            },
        )
    }

    /// Explicitly advance completions with a short bounded progress assertion.
    fn drive<T>(files: &Files, mut operation: Operation<'_, T, Failure>) -> Result<T, Failure> {
        let deadline = Instant::now() + Duration::from_secs(5);
        loop {
            if let Poll::Ready(result) = operation
                .as_mut()
                .poll(&mut Context::from_waker(futures::task::noop_waker_ref()))
            {
                return result;
            }
            assert!(Instant::now() < deadline, "completion progress deadline");
            files.reactor.poll_budgeted(64).unwrap();
            files.reactor.wait(Duration::from_millis(1)).unwrap();
        }
    }

    /// Issue a bounded client certificate for the exact CSR, with optional policy mutations.
    fn issue(
        request: &EnrollmentRequest,
        ca: &rcgen::Certificate,
        key: &rcgen::KeyPair,
        customize: impl FnOnce(&mut rcgen::CertificateParams),
    ) -> EnrollmentResponse {
        let der = rustls::pki_types::CertificateSigningRequestDer::from(request.csr_der.clone());
        let mut csr = rcgen::CertificateSigningRequestParams::from_der(&der).unwrap();
        let now = uring_runtime::environment::wall_now();
        csr.params.not_before = (now - Duration::from_secs(1)).into();
        csr.params.not_after = (now + Duration::from_secs(3600)).into();
        csr.params.subject_alt_names = vec![rcgen::SanType::URI(
            format!("spiffe://{CLUSTER}/node/{NODE}")
                .try_into()
                .unwrap(),
        )];
        csr.params.key_usages = vec![rcgen::KeyUsagePurpose::DigitalSignature];
        csr.params.extended_key_usages = vec![rcgen::ExtendedKeyUsagePurpose::ClientAuth];
        customize(&mut csr.params);
        EnrollmentResponse {
            block_devices: None,
            schema_version: 1,
            cluster: request.cluster.clone(),
            node: NodeId(NODE.into()),
            enrollment: request.enrollment.clone(),
            certificate_chain: vec![csr.signed_by(ca, key).unwrap().der().to_vec()],
        }
    }

    /// Enrollment keeps an empty subject and signs with the persisted identity key.
    #[test]
    fn generated_csr_public_key_and_signature_match_persisted_identity() {
        use x509_parser::{certification_request::X509CertificationRequest, prelude::FromDer};

        let sim = Simulation::new();
        let _environment = sim.enter();
        let (_, enrollment, _) = fixture();
        let pending = enrollment.generate().unwrap();
        let private = decode_private_key(&pending.private_key).unwrap();
        let key = crate::SigningKey::from_pkcs8_der(&private).unwrap();
        let der = STANDARD.decode(&pending.csr).unwrap();
        let (rest, csr) = X509CertificationRequest::from_der(&der).unwrap();
        assert!(rest.is_empty());
        let info = &csr.certification_request_info;
        assert_eq!(info.subject.iter().count(), 0);
        assert_eq!(
            info.subject_pki.subject_public_key.data.as_ref(),
            key.verifying_key().as_bytes()
        );
        assert_eq!(
            csr.signature_algorithm.algorithm.to_id_string(),
            "1.3.101.112"
        );
        csr.verify_signature().unwrap();
    }

    /// Retry persistence, inventory refresh, token rotation and recovery share one path.
    #[test]
    fn durable_enrollment_retries_rotation_and_recovery() {
        let sim = Simulation::new();
        let _environment = sim.enter();
        let (files, e, scope) = fixture();
        assert!(drive(&files, e.load_identity(&scope)).unwrap().is_none());
        let request = drive(
            &files,
            e.prepare(vec![], NonZeroU32::new(4).unwrap(), &scope),
        )
        .unwrap();
        sim.disk().crash().unwrap();
        let nics = vec![RailMapping {
            device: "nic-a".into(),
            port: 1,
            rail: wire::RailId(7),
            gid: Some([1; 16]),
            numa_node: None,
        }];
        let retry = drive(
            &files,
            e.prepare(nics.clone(), NonZeroU32::new(8).unwrap(), &scope),
        )
        .unwrap();
        assert_eq!(retry.enrollment, request.enrollment);
        assert_eq!(retry.csr_der, request.csr_der);
        assert_eq!(retry.shares, 8);
        assert_eq!(retry.rdma_nics, nics);
        use x509_parser::prelude::FromDer;
        let (_, csr) =
            x509_parser::certification_request::X509CertificationRequest::from_der(&retry.csr_der)
                .unwrap();
        assert_eq!(csr.certification_request_info.subject.iter().count(), 0);
        assert!(sim.metadata(Path::new("/private/rdma-rails.json")).is_err());
        sim.write_file(Path::new("/token-data"), b" first.token\n")
            .unwrap();
        sim.symlink(Path::new("token-data"), Path::new("/token"))
            .unwrap();
        assert_eq!(
            &*drive(&files, e.read_token(&scope)).unwrap(),
            "first.token"
        );
        sim.write_file(Path::new("/token-data"), b"second.token")
            .unwrap();
        assert_eq!(
            &*drive(&files, e.read_token(&scope)).unwrap(),
            "second.token"
        );
        let (ca, key) = identity::test_util::ca();
        e.set_peer_trust_roots(vec![ca.der().to_vec()]).unwrap();
        let pending = sim.read_file(Path::new("/private/pending.json")).unwrap();
        let identity = drive(
            &files,
            e.accept_response(issue(&request, &ca, &key, |_| {}), &scope),
        )
        .unwrap();
        assert!(identity.valid_now());
        assert!(!identity.renewal_due());
        assert!(!identity.private_key_der().is_empty());
        assert!(identity.signing_identity(&[ca.der().to_vec()]).is_ok());
        sim.disk().crash().unwrap();
        assert_eq!(
            drive(&files, e.load_identity(&scope))
                .unwrap()
                .unwrap()
                .node(),
            identity.node()
        );
        // Recovery cleans up a published identity's matching pending record only.
        sim.write_file(Path::new("/private/pending.json"), &pending)
            .unwrap();
        sim.chmod(Path::new("/private/pending.json"), 0o600)
            .unwrap();
        drive(&files, e.load_identity(&scope)).unwrap().unwrap();
        assert!(sim.metadata(Path::new("/private/pending.json")).is_err());
        let fresh = drive(
            &files,
            e.prepare(vec![], NonZeroU32::new(4).unwrap(), &scope),
        )
        .unwrap();
        assert_ne!(fresh.enrollment, request.enrollment);
        drive(&files, e.load_identity(&scope)).unwrap().unwrap();
        assert!(sim.metadata(Path::new("/private/pending.json")).is_ok());
    }

    #[test]
    fn identity_recovery_preserves_undecodable_and_unrelated_pending() {
        let sim = Simulation::new();
        let _environment = sim.enter();
        let (files, e, scope) = fixture();
        let (ca, key) = identity::test_util::ca();
        e.set_peer_trust_roots(vec![ca.der().to_vec()]).unwrap();
        let request = drive(
            &files,
            e.prepare(vec![], NonZeroU32::new(4).unwrap(), &scope),
        )
        .unwrap();
        let path = Path::new("/private/pending.json");
        let matching = sim.read_file(path).unwrap();
        let accepted = drive(
            &files,
            e.accept_response(issue(&request, &ca, &key, |_| {}), &scope),
        )
        .unwrap();
        let unrelated = e.generate().unwrap();
        assert_ne!(unrelated.enrollment, accepted.enrollment.0);
        let unrelated = encode_private(&unrelated, wire::MAX_ENROLLMENT_BYTES).unwrap();
        for (pending, removed) in [
            (b"".as_slice(), false),
            (b"not JSON".as_slice(), false),
            (b"{\"enrollment\":".as_slice(), false),
            (b"{}".as_slice(), false),
            (matching.as_slice(), true),
            (unrelated.as_slice(), false),
        ] {
            sim.write_file(path, pending).unwrap();
            sim.chmod(path, 0o600).unwrap();
            let recovered = drive(&files, e.load_identity(&scope)).unwrap().unwrap();
            assert_eq!(recovered.node(), accepted.node());
            assert_eq!(recovered.enrollment, accepted.enrollment);
            assert_eq!(recovered.certificate_chain(), accepted.certificate_chain());
            if removed {
                assert!(sim.metadata(path).is_err());
            } else {
                assert_eq!(sim.read_file(path).unwrap(), pending);
            }
        }
    }

    /// Live acceptance and a fresh owner recover the same optional selector.
    #[test]
    fn block_device_selector_survives_acceptance_and_recovery() {
        for selector in [Some(r"^nvme-eui\.[0-9a-f]+$"), None, Some("")] {
            let sim = Simulation::new();
            let _environment = sim.enter();
            let (files, e, scope) = fixture();
            let (ca, key) = identity::test_util::ca();
            let roots = vec![ca.der().to_vec()];
            e.set_peer_trust_roots(roots.clone()).unwrap();
            let request = drive(
                &files,
                e.prepare(vec![], NonZeroU32::new(4).unwrap(), &scope),
            )
            .unwrap();
            let mut response = issue(&request, &ca, &key, |_| {});
            response.block_devices = selector.map(str::to_owned);
            let expected = selector.filter(|value| !value.is_empty());
            let accepted = drive(&files, e.accept_response(response, &scope)).unwrap();
            assert_eq!(accepted.block_devices(), expected);
            drop(e);
            drop(files);
            sim.disk().crash().unwrap();
            let (files, e, scope) = fixture();
            e.set_peer_trust_roots(roots).unwrap();
            let recovered = drive(&files, e.load_identity(&scope)).unwrap().unwrap();
            assert_eq!(recovered.block_devices(), expected);
            assert_eq!(recovered.node(), accepted.node());
            assert_eq!(recovered.certificate_chain(), accepted.certificate_chain());
        }
    }

    /// Rejected renewals retain the selector; accepted renewals replace or clear it.
    #[test]
    fn block_device_selector_changes_only_after_accepted_renewal() {
        for selector in [Some("new-device.*"), None, Some("")] {
            let sim = Simulation::new();
            let _environment = sim.enter();
            let (files, e, scope) = fixture();
            let (ca, key) = identity::test_util::ca();
            e.set_peer_trust_roots(vec![ca.der().to_vec()]).unwrap();
            let first = drive(
                &files,
                e.prepare(vec![], NonZeroU32::new(4).unwrap(), &scope),
            )
            .unwrap();
            let mut response = issue(&first, &ca, &key, |_| {});
            response.block_devices = Some("old-device.*".into());
            let prior = drive(&files, e.accept_response(response, &scope)).unwrap();
            let request = drive(
                &files,
                e.prepare(vec![], NonZeroU32::new(4).unwrap(), &scope),
            )
            .unwrap();
            let committed = sim.read_file(Path::new("/private/identity.json")).unwrap();
            let pending = sim.read_file(Path::new("/private/pending.json")).unwrap();
            let mut response = issue(&request, &ca, &key, |_| {});
            response.block_devices = selector.map(str::to_owned);
            let mut rejected = response.clone();
            rejected.enrollment = first.enrollment;
            assert!(matches!(
                drive(&files, e.accept_response(rejected, &scope)),
                Err(error) if error == Error::Unauthorized.into()
            ));
            sim.disk().crash().unwrap();
            assert_eq!(
                sim.read_file(Path::new("/private/identity.json")).unwrap(),
                committed
            );
            assert_eq!(
                sim.read_file(Path::new("/private/pending.json")).unwrap(),
                pending
            );
            let recovered = drive(&files, e.load_identity(&scope)).unwrap().unwrap();
            assert_eq!(recovered.block_devices(), Some("old-device.*"));
            assert_eq!(recovered.certificate_chain(), prior.certificate_chain());
            let renewed = drive(&files, e.accept_response(response, &scope)).unwrap();
            let expected = selector.filter(|value| !value.is_empty());
            assert_eq!(renewed.block_devices(), expected);
            assert_eq!(prior.block_devices(), Some("old-device.*"));
            sim.disk().crash().unwrap();
            let recovered = drive(&files, e.load_identity(&scope)).unwrap().unwrap();
            assert_eq!(recovered.block_devices(), expected);
            assert_eq!(recovered.certificate_chain(), renewed.certificate_chain());
        }
    }

    /// Late base64 failures preserve pending work and never publish or recover a key.
    #[test]
    fn late_private_key_base64_errors_preserve_identity_records() {
        let sim = Simulation::new();
        let _environment = sim.enter();
        let (files, e, scope) = fixture();
        let request = drive(
            &files,
            e.prepare(vec![], NonZeroU32::new(4).unwrap(), &scope),
        )
        .unwrap();
        let (ca, key) = identity::test_util::ca();
        e.set_peer_trust_roots(vec![ca.der().to_vec()]).unwrap();
        let response = issue(&request, &ca, &key, |_| {});
        let original = sim.read_file(Path::new("/private/pending.json")).unwrap();
        let pending = decode_pending(&original).unwrap();
        let decoded = decode_private_key(&pending.private_key).unwrap();
        assert_eq!(STANDARD.encode(&*decoded), *pending.private_key.0);
        assert!(decoded.len() > 32);
        for suffix in ["!AAA", "A===", "AA=A"] {
            let mut pending = decode_pending(&original).unwrap();
            // Leave complete valid groups before corrupting only the final group.
            let length = pending.private_key.0.len();
            pending.private_key.0.replace_range(length - 4.., suffix);
            assert!(matches!(
                decode_private_key(&pending.private_key),
                Err(Error::CorruptRecord)
            ));
            let corrupted = encode_private(&pending, wire::MAX_ENROLLMENT_BYTES).unwrap();
            sim.write_file(Path::new("/private/pending.json"), &corrupted)
                .unwrap();
            assert!(matches!(
                drive(&files, e.prepare(vec![], NonZeroU32::new(4).unwrap(), &scope)),
                Err(error) if error == Error::CorruptRecord.into()
            ));
            assert!(matches!(
                drive(&files, e.accept_response(response.clone(), &scope)),
                Err(error) if error == Error::CorruptRecord.into()
            ));
            assert!(sim.metadata(Path::new("/private/identity.json")).is_err());
            assert_eq!(
                sim.read_file(Path::new("/private/pending.json")).unwrap(),
                *corrupted
            );

            // Recovery reaches validate's decoder independently of the CSR check.
            let persisted = PersistedIdentity {
                pending,
                response: STANDARD.encode(wire::encode_enrollment_response(&response).unwrap()),
            }
            .encode()
            .unwrap();
            sim.write_file(Path::new("/private/identity.json"), &persisted)
                .unwrap();
            sim.chmod(Path::new("/private/identity.json"), 0o600)
                .unwrap();
            assert!(
                matches!(drive(&files, e.load_identity(&scope)), Err(error) if error == Error::CorruptRecord.into())
            );
            assert_eq!(
                sim.read_file(Path::new("/private/identity.json")).unwrap(),
                *persisted
            );
            assert_eq!(
                sim.read_file(Path::new("/private/pending.json")).unwrap(),
                *corrupted
            );
            sim.unlink(Path::new("/private/identity.json")).unwrap();
        }
        let oversized =
            EncodedPrivateKey(Zeroizing::new("A".repeat(wire::MAX_ENROLLMENT_BYTES + 1)));
        assert!(matches!(
            decode_private_key(&oversized),
            Err(Error::CorruptRecord)
        ));
        sim.write_file(Path::new("/private/pending.json"), &original)
            .unwrap();
        assert!(drive(&files, e.accept_response(response, &scope)).is_ok());
        assert!(sim.metadata(Path::new("/private/pending.json")).is_err());
    }

    /// Invalid certificates and pending material never publish an identity.
    #[test]
    fn certificate_correlation_key_pairing_and_pending_corruption_fail_closed() {
        let sim = Simulation::new();
        let _environment = sim.enter();
        let (files, e, scope) = fixture();
        let request = drive(
            &files,
            e.prepare(vec![], NonZeroU32::new(4).unwrap(), &scope),
        )
        .unwrap();
        let (ca, key) = identity::test_util::ca();
        e.set_peer_trust_roots(vec![ca.der().to_vec()]).unwrap();
        for case in ["san", "lifetime", "usage", "id", "key", "cluster", "trust"] {
            let mut response = issue(&request, &ca, &key, |params| match case {
                "san" => params
                    .subject_alt_names
                    .push(rcgen::SanType::DnsName("extra.example".try_into().unwrap())),
                "lifetime" => {
                    params.not_after =
                        (uring_runtime::environment::wall_now() + Duration::from_secs(90000)).into()
                }
                "usage" => {
                    params.extended_key_usages = vec![rcgen::ExtendedKeyUsagePurpose::ServerAuth]
                }
                _ => (),
            });
            if case == "id" {
                response.enrollment = EnrollmentId(NODE.into());
            }
            if case == "cluster" {
                response.cluster = ClusterId(NODE.into());
            }
            if case == "key" {
                let other = e
                    .request(&e.generate().unwrap(), vec![], NonZeroU32::new(4).unwrap())
                    .unwrap();
                response.certificate_chain = issue(&other, &ca, &key, |_| {}).certificate_chain;
            }
            if case == "trust" {
                let (rogue, rogue_key) = identity::test_util::ca();
                response = issue(&request, &rogue, &rogue_key, |_| {});
            }
            assert!(
                matches!(drive(&files, e.accept_response(response, &scope)), Err(error) if error == Error::Unauthorized.into()),
                "{case}"
            );
            assert!(sim.metadata(Path::new("/private/identity.json")).is_err());
        }
        sim.write_file(Path::new("/private/pending.json"), b"{}")
            .unwrap();
        assert!(
            matches!(drive(&files, e.prepare(vec![], NonZeroU32::new(4).unwrap(), &scope)), Err(error) if error == Error::CorruptRecord.into())
        );
        for invalid in [b"".as_slice(), b"a b", b"\xff", b"token\r\nheader"] {
            assert!(matches!(token(invalid), Err(Error::Unauthorized)));
        }
    }

    /// Activation policy rejects renewals before replacing a usable identity or CSR.
    #[test]
    fn rejected_renewal_preserves_prior_identity_and_pending_request() {
        let sim = Simulation::new();
        let _environment = sim.enter();
        let (files, e, scope) = fixture();
        let (ca, key) = identity::test_util::ca();
        let roots = vec![ca.der().to_vec()];
        e.set_peer_trust_roots(roots.clone()).unwrap();
        let first = drive(
            &files,
            e.prepare(vec![], NonZeroU32::new(4).unwrap(), &scope),
        )
        .unwrap();
        let prior = drive(
            &files,
            e.accept_response(issue(&first, &ca, &key, |_| {}), &scope),
        )
        .unwrap();
        let request = drive(
            &files,
            e.prepare(vec![], NonZeroU32::new(4).unwrap(), &scope),
        )
        .unwrap();
        let committed = sim.read_file(Path::new("/private/identity.json")).unwrap();
        let pending = sim.read_file(Path::new("/private/pending.json")).unwrap();
        for case in [
            "key-cert-sign",
            "key-encipherment",
            "server-auth",
            "unknown-eku",
            "any-eku",
            "ca",
            "missing-usage",
            "future",
            "expired",
        ] {
            let response = issue(&request, &ca, &key, |params| match case {
                "key-cert-sign" => params.key_usages.push(rcgen::KeyUsagePurpose::KeyCertSign),
                "key-encipherment" => params
                    .key_usages
                    .push(rcgen::KeyUsagePurpose::KeyEncipherment),
                "server-auth" => params
                    .extended_key_usages
                    .push(rcgen::ExtendedKeyUsagePurpose::ServerAuth),
                "unknown-eku" => params
                    .extended_key_usages
                    .push(rcgen::ExtendedKeyUsagePurpose::Other(vec![1, 2, 3, 4])),
                "any-eku" => params
                    .extended_key_usages
                    .push(rcgen::ExtendedKeyUsagePurpose::Any),
                "ca" => params.is_ca = rcgen::IsCa::Ca(rcgen::BasicConstraints::Unconstrained),
                "missing-usage" => params.key_usages.clear(),
                "future" => {
                    params.not_before =
                        (uring_runtime::environment::wall_now() + Duration::from_secs(600)).into()
                }
                "expired" => {
                    params.not_before =
                        (uring_runtime::environment::wall_now() - Duration::from_secs(600)).into();
                    params.not_after =
                        (uring_runtime::environment::wall_now() - Duration::from_secs(300)).into();
                }
                _ => unreachable!(),
            });
            assert!(
                matches!(drive(&files, e.accept_response(response.clone(), &scope)), Err(error) if error == Error::Unauthorized.into()),
                "{case}"
            );
            assert_eq!(
                sim.read_file(Path::new("/private/identity.json")).unwrap(),
                committed,
                "{case}"
            );
            assert_eq!(
                sim.read_file(Path::new("/private/pending.json")).unwrap(),
                pending,
                "{case}"
            );
            let recovered = drive(&files, e.load_identity(&scope)).unwrap().unwrap();
            assert_eq!(recovered.certificate_chain(), prior.certificate_chain());
            assert!(
                recovered
                    .signing_identity(&roots)
                    .unwrap()
                    .sign(b"still usable")
                    .is_ok()
            );
            // Legacy records written by a weaker version also fail closed on load,
            // without deleting the matching pending request during recovery.
            let invalid = PersistedIdentity {
                pending: decode_pending(&pending).unwrap(),
                response: STANDARD.encode(wire::encode_enrollment_response(&response).unwrap()),
            };
            sim.write_file(
                Path::new("/private/identity.json"),
                &invalid.encode().unwrap(),
            )
            .unwrap();
            assert!(
                drive(&files, e.load_identity(&scope)).unwrap().is_none(),
                "{case}"
            );
            assert_eq!(
                sim.read_file(Path::new("/private/pending.json")).unwrap(),
                pending
            );
            sim.write_file(Path::new("/private/identity.json"), &committed)
                .unwrap();
        }
        let accepted = drive(
            &files,
            e.accept_response(issue(&request, &ca, &key, |_| {}), &scope),
        )
        .unwrap();
        assert!(accepted.signing_identity(&roots).is_ok());
        assert!(sim.metadata(Path::new("/private/pending.json")).is_err());
    }

    /// A valid renewal cannot move a durable identity to another node UID.
    #[test]
    fn renewal_requires_node_continuity_before_durable_publication() {
        let sim = Simulation::new();
        let _environment = sim.enter();
        let (files, e, scope) = fixture();
        let (ca, key) = identity::test_util::ca();
        e.set_peer_trust_roots(vec![ca.der().to_vec()]).unwrap();
        let first = drive(
            &files,
            e.prepare(vec![], NonZeroU32::new(4).unwrap(), &scope),
        )
        .unwrap();
        let prior = drive(
            &files,
            e.accept_response(issue(&first, &ca, &key, |_| {}), &scope),
        )
        .unwrap();
        let request = drive(
            &files,
            e.prepare(vec![], NonZeroU32::new(4).unwrap(), &scope),
        )
        .unwrap();
        assert_ne!(request.enrollment, first.enrollment);
        let committed = sim.read_file(Path::new("/private/identity.json")).unwrap();
        let pending = sim.read_file(Path::new("/private/pending.json")).unwrap();
        let other_node = "33333333-3333-4333-8333-333333333333";
        let mut response = issue(&request, &ca, &key, |params| {
            params.subject_alt_names = vec![rcgen::SanType::URI(
                format!("spiffe://{CLUSTER}/node/{other_node}")
                    .try_into()
                    .unwrap(),
            )];
        });
        response.node = NodeId(other_node.into());
        assert!(e.accept_pending(&pending, &response).is_ok());
        assert!(matches!(
            drive(&files, e.accept_response(response, &scope)),
            Err(error) if error == Error::Unauthorized.into()
        ));
        for crashed in [false, true] {
            if crashed {
                sim.disk().crash().unwrap();
            }
            assert_eq!(
                sim.read_file(Path::new("/private/identity.json")).unwrap(),
                committed
            );
            assert_eq!(
                sim.read_file(Path::new("/private/pending.json")).unwrap(),
                pending
            );
            let recovered = drive(&files, e.load_identity(&scope)).unwrap().unwrap();
            assert_eq!(recovered.node(), prior.node());
            assert_eq!(recovered.certificate_chain(), prior.certificate_chain());
            assert_eq!(recovered.private_key_der(), prior.private_key_der());
        }
        let renewed = drive(
            &files,
            e.accept_response(issue(&request, &ca, &key, |_| {}), &scope),
        )
        .unwrap();
        assert_eq!(renewed.node(), prior.node());
        assert_eq!(renewed.enrollment, request.enrollment);
        assert_ne!(renewed.private_key_der(), prior.private_key_der());
        sim.disk().crash().unwrap();
        assert!(sim.metadata(Path::new("/private/pending.json")).is_err());
        let recovered = drive(&files, e.load_identity(&scope)).unwrap().unwrap();
        assert_eq!(recovered.node(), renewed.node());
        assert_eq!(recovered.enrollment, renewed.enrollment);
        assert_eq!(recovered.certificate_chain(), renewed.certificate_chain());
        assert_eq!(recovered.private_key_der(), renewed.private_key_der());
    }

    /// Every root is checked before configuration and again before persistence/load.
    #[test]
    fn invalid_root_snapshots_preserve_last_good_identity_and_trust() {
        let sim = Simulation::new();
        let _environment = sim.enter();
        let (files, e, scope) = fixture();
        let (ca, key) = identity::test_util::ca();
        let good = vec![ca.der().to_vec()];
        e.set_peer_trust_roots(good.clone()).unwrap();
        let first = drive(
            &files,
            e.prepare(vec![], NonZeroU32::new(4).unwrap(), &scope),
        )
        .unwrap();
        let prior = drive(
            &files,
            e.accept_response(issue(&first, &ca, &key, |_| {}), &scope),
        )
        .unwrap();
        let request = drive(
            &files,
            e.prepare(vec![], NonZeroU32::new(4).unwrap(), &scope),
        )
        .unwrap();
        let response = issue(&request, &ca, &key, |_| {});
        let committed = sim.read_file(Path::new("/private/identity.json")).unwrap();
        let pending = sim.read_file(Path::new("/private/pending.json")).unwrap();
        for case in [
            "expired",
            "future",
            "non-ca",
            "trailing",
            "empty",
            "too-many",
            "oversized",
        ] {
            let now = uring_runtime::environment::wall_now();
            let mut params = rcgen::CertificateParams::new(Vec::<String>::new()).unwrap();
            params.is_ca = rcgen::IsCa::Ca(rcgen::BasicConstraints::Unconstrained);
            params.not_before = (now - Duration::from_secs(600)).into();
            params.not_after = (now + Duration::from_secs(600)).into();
            match case {
                "expired" => params.not_after = (now - Duration::from_secs(300)).into(),
                "future" => params.not_before = (now + Duration::from_secs(300)).into(),
                "non-ca" => params.is_ca = rcgen::IsCa::ExplicitNoCa,
                _ => (),
            }
            let mut bad = params.self_signed(&key).unwrap().der().to_vec();
            if case == "trailing" {
                bad.push(0);
            }
            if case == "oversized" {
                bad.resize(16385, 0);
            }
            let roots = match case {
                "empty" => vec![],
                "too-many" => vec![ca.der().to_vec(); 33],
                // Even an unused bad anchor must not weaken the activation policy.
                _ => vec![ca.der().to_vec(), bad],
            };
            assert_eq!(
                e.set_peer_trust_roots(roots.clone()),
                Err(Error::Unauthorized),
                "{case}"
            );
            assert_eq!(*e.roots.borrow(), good);
            // Exercise legacy invalid snapshots independently of setter rejection.
            *e.roots.borrow_mut() = roots;
            assert!(
                matches!(drive(&files, e.accept_response(response.clone(), &scope)), Err(error) if error == Error::Unauthorized.into()),
                "{case}"
            );
            assert!(
                drive(&files, e.load_identity(&scope)).unwrap().is_none(),
                "{case}"
            );
            assert_eq!(
                sim.read_file(Path::new("/private/identity.json")).unwrap(),
                committed
            );
            assert_eq!(
                sim.read_file(Path::new("/private/pending.json")).unwrap(),
                pending
            );
            e.set_peer_trust_roots(good.clone()).unwrap();
            let recovered = drive(&files, e.load_identity(&scope)).unwrap().unwrap();
            assert_eq!(recovered.certificate_chain(), prior.certificate_chain());
            assert!(
                recovered
                    .signing_identity(&good)
                    .unwrap()
                    .sign(b"still usable")
                    .is_ok()
            );
        }
    }

    /// Trust cannot change after validation while publication or cleanup awaits I/O.
    #[test]
    fn trust_rotation_is_excluded_until_acceptance_or_recovery_releases() {
        for recovering in [false, true] {
            for finish in ["complete", "abandon", "cancel", "failure"] {
                let sim = Simulation::new();
                let _environment = sim.enter();
                let (files, e, scope) = fixture();
                let (ca, key) = identity::test_util::ca();
                let good = vec![ca.der().to_vec()];
                let (other, _) = identity::test_util::ca();
                let rotated = vec![other.der().to_vec()];
                e.set_peer_trust_roots(good.clone()).unwrap();
                let request = drive(
                    &files,
                    e.prepare(vec![], NonZeroU32::new(4).unwrap(), &scope),
                )
                .unwrap();
                let pending = sim.read_file(Path::new("/private/pending.json")).unwrap();
                let response = issue(&request, &ca, &key, |_| {});
                if recovering {
                    drive(&files, e.accept_response(response.clone(), &scope)).unwrap();
                    sim.write_file(Path::new("/private/pending.json"), &pending)
                        .unwrap();
                    sim.chmod(Path::new("/private/pending.json"), 0o600)
                        .unwrap();
                }
                sim.inject(
                    if recovering { "unlink" } else { "rename" },
                    Fault::HoldCompletion(20),
                )
                .unwrap();
                let mut operation = Box::pin(async {
                    if recovering {
                        e.load_identity(&scope).await.map(Option::unwrap)
                    } else {
                        e.accept_response(response, &scope).await
                    }
                });
                let mut paused = false;
                for _ in 0..100 {
                    assert!(
                        operation
                            .as_mut()
                            .poll(&mut Context::from_waker(futures::task::noop_waker_ref()))
                            .is_pending()
                    );
                    files.reactor.poll_budgeted(64).unwrap();
                    files.reactor.wait(Duration::from_millis(1)).unwrap();
                    paused = if recovering {
                        sim.metadata(Path::new("/private/pending.json")).is_err()
                    } else {
                        sim.metadata(Path::new("/private/identity.json")).is_ok()
                    };
                    if paused {
                        break;
                    }
                }
                assert!(paused, "recovering={recovering}, finish={finish}");
                // Both paths have validated and submitted a filesystem mutation.
                assert_eq!(e.set_peer_trust_roots(rotated.clone()), Err(Error::Busy));
                assert_eq!(*e.roots.borrow(), good);
                // Rejected setters cannot release another operation's guard.
                assert_eq!(e.set_peer_trust_roots(good.clone()), Err(Error::Busy));
                match finish {
                    "abandon" => drop(operation),
                    "cancel" => {
                        scope.cancellation.cancel().unwrap();
                        assert!(matches!(drive(&files, operation), Err(error)
                            if error.cause == Cause::Runtime(uring_runtime::Error::Cancelled)));
                    }
                    "failure" => {
                        sim.inject("fsync", Fault::Errno(5)).unwrap();
                        assert!(matches!(drive(&files, operation), Err(error)
                            if error.cause == Cause::Runtime(uring_runtime::Error::Os(5))));
                    }
                    _ => {
                        let accepted = drive(&files, operation).unwrap();
                        assert!(accepted.signing_identity(&good).is_ok());
                    }
                }
                e.set_peer_trust_roots(rotated.clone()).unwrap();
                assert_eq!(*e.roots.borrow(), rotated);
                let scope = Request {
                    id: 0,
                    cancellation: Cancellation::new().unwrap(),
                };
                // Recovery fences abandoned I/O, then applies the new trust roots.
                assert!(drive(&files, e.load_identity(&scope)).unwrap().is_none());
                assert_eq!(files.reactor.in_flight(), 0);
                e.set_peer_trust_roots(good.clone()).unwrap();
                let recovered = drive(&files, e.load_identity(&scope)).unwrap().unwrap();
                assert!(recovered.signing_identity(&good).is_ok());
                assert!(sim.metadata(Path::new("/private/pending.json")).is_err());
            }
        }
    }

    /// Trust validity must be rechecked when a previously accepted anchor expires.
    #[test]
    fn root_expiring_after_installation_blocks_persistence_and_recovery() {
        let sim = Simulation::new();
        let _environment = sim.enter();
        let clock = uring_runtime::environment::SimulationClock::new(105);
        let _time = clock.environment(0).enter();
        let (files, e, scope) = fixture();
        let (ca, key) = identity::test_util::ca();
        let good = vec![ca.der().to_vec()];
        e.set_peer_trust_roots(good.clone()).unwrap();
        let first = drive(
            &files,
            e.prepare(vec![], NonZeroU32::new(4).unwrap(), &scope),
        )
        .unwrap();
        let prior = drive(
            &files,
            e.accept_response(issue(&first, &ca, &key, |_| {}), &scope),
        )
        .unwrap();
        let request = drive(
            &files,
            e.prepare(vec![], NonZeroU32::new(4).unwrap(), &scope),
        )
        .unwrap();
        let committed = sim.read_file(Path::new("/private/identity.json")).unwrap();
        let pending = sim.read_file(Path::new("/private/pending.json")).unwrap();
        let mut params = rcgen::CertificateParams::new(Vec::<String>::new()).unwrap();
        params.is_ca = rcgen::IsCa::Ca(rcgen::BasicConstraints::Unconstrained);
        let now = uring_runtime::environment::wall_now();
        params.not_before = (now - Duration::from_secs(60)).into();
        params.not_after = (now + Duration::from_secs(60)).into();
        let short = params.self_signed(&key).unwrap();
        e.set_peer_trust_roots(vec![ca.der().to_vec(), short.der().to_vec()])
            .unwrap();
        clock.advance(Duration::from_secs(120));
        assert!(
            matches!(drive(&files, e.accept_response(issue(&request, &ca, &key, |_| {}), &scope)), Err(error) if error == Error::Unauthorized.into())
        );
        assert!(drive(&files, e.load_identity(&scope)).unwrap().is_none());
        assert_eq!(
            sim.read_file(Path::new("/private/identity.json")).unwrap(),
            committed
        );
        assert_eq!(
            sim.read_file(Path::new("/private/pending.json")).unwrap(),
            pending
        );
        e.set_peer_trust_roots(good.clone()).unwrap();
        let recovered = drive(&files, e.load_identity(&scope)).unwrap().unwrap();
        assert_eq!(recovered.certificate_chain(), prior.certificate_chain());
        assert!(
            recovered
                .signing_identity(&good)
                .unwrap()
                .sign(b"still usable")
                .is_ok()
        );
    }

    /// Held accepted and recovered identities must stay within the root's lifetime.
    #[test]
    fn held_enrollment_identity_uses_root_validity_bounds() {
        let sim = Simulation::new();
        let _environment = sim.enter();
        let clock = uring_runtime::environment::SimulationClock::new(106);
        let _time = clock.environment(0).enter();
        let (files, e, scope) = fixture();
        let now = uring_runtime::environment::wall_now();
        let (_, key) = identity::test_util::ca();
        let mut params = rcgen::CertificateParams::new(Vec::<String>::new()).unwrap();
        params.is_ca = rcgen::IsCa::Ca(rcgen::BasicConstraints::Unconstrained);
        params.not_before = now.into();
        params.not_after = (now + Duration::from_secs(60)).into();
        let ca = params.self_signed(&key).unwrap();
        e.set_peer_trust_roots(vec![ca.der().to_vec()]).unwrap();
        let request = drive(
            &files,
            e.prepare(vec![], NonZeroU32::new(4).unwrap(), &scope),
        )
        .unwrap();
        let response = issue(&request, &ca, &key, |_| {});
        let (_, leaf) =
            x509_parser::parse_x509_certificate(&response.certificate_chain[0]).unwrap();
        let leaf_start = UNIX_EPOCH
            + Duration::from_secs(leaf.validity().not_before.timestamp().try_into().unwrap());
        let leaf_end = UNIX_EPOCH
            + Duration::from_secs(leaf.validity().not_after.timestamp().try_into().unwrap());
        assert!(leaf_start < now);
        assert!(leaf_end > now + Duration::from_secs(60));
        let accepted = drive(&files, e.accept_response(response, &scope)).unwrap();
        let recovered = drive(&files, e.load_identity(&scope)).unwrap().unwrap();
        for identity in [&accepted, &recovered] {
            assert_eq!(identity.expires_at(), now + Duration::from_secs(60));
            clock.set_wall_time(now - Duration::from_secs(1));
            assert!(!identity.valid_now());
            clock.set_wall_time(now);
            assert!(identity.valid_now());
            assert!(!identity.renewal_due());
            clock.set_wall_time(now + Duration::from_secs(39));
            assert!(!identity.renewal_due());
            clock.set_wall_time(now + Duration::from_secs(40));
            assert!(identity.renewal_due());
            clock.set_wall_time(now + Duration::from_secs(59));
            assert!(identity.valid_now());
            clock.set_wall_time(now + Duration::from_secs(60));
            assert!(!identity.valid_now());
            clock.set_wall_time(now + Duration::from_secs(61));
            assert!(!identity.valid_now());
            assert!(uring_runtime::environment::wall_now() < leaf_end);
        }
        assert!(drive(&files, e.load_identity(&scope)).unwrap().is_none());
    }

    /// Abandoned writes are fenced before retry and publication phases stay exact.
    #[test]
    fn persistence_faults_abandonment_and_reconciliation_preserve_errors() {
        for case in ["write", "rename", "sync", "abandon", "cancel-rename"] {
            let sim = Simulation::new();
            let _environment = sim.enter();
            let (files, e, scope) = fixture();
            let request = drive(
                &files,
                e.prepare(vec![], NonZeroU32::new(4).unwrap(), &scope),
            )
            .unwrap();
            let (ca, key) = identity::test_util::ca();
            e.set_peer_trust_roots(vec![ca.der().to_vec()]).unwrap();
            match case {
                "write" => sim.inject("write", Fault::Errno(28)).unwrap(),
                "rename" => sim.inject("rename", Fault::Errno(5)).unwrap(),
                _ => sim.inject("rename", Fault::HoldCompletion(20)).unwrap(),
            }
            let mut accept = e.accept_response(issue(&request, &ca, &key, |_| {}), &scope);
            if matches!(case, "sync" | "abandon" | "cancel-rename") {
                let mut published = false;
                for _ in 0..100 {
                    assert!(
                        accept
                            .as_mut()
                            .poll(&mut Context::from_waker(futures::task::noop_waker_ref()))
                            .is_pending()
                    );
                    files.reactor.poll_budgeted(64).unwrap();
                    files.reactor.wait(Duration::from_millis(1)).unwrap();
                    if sim.metadata(Path::new("/private/identity.json")).is_ok() {
                        published = true;
                        break;
                    }
                }
                assert!(published);
            }
            if case == "abandon" {
                drop(accept);
                assert!(drive(&files, e.load_identity(&scope)).unwrap().is_some());
            } else {
                if case == "sync" {
                    sim.inject("fsync", Fault::Errno(5)).unwrap();
                }
                if case == "cancel-rename" {
                    scope.cancellation.cancel().unwrap();
                }
                let expected = Failure {
                    cause: Cause::Runtime(if case == "cancel-rename" {
                        uring_runtime::Error::Cancelled
                    } else {
                        uring_runtime::Error::Os(if case == "write" { 28 } else { 5 })
                    }),
                    phase: match case {
                        "rename" | "cancel-rename" => 1,
                        "sync" => 2,
                        _ => 0,
                    },
                };
                assert!(
                    matches!(drive(&files, accept), Err(actual) if actual == expected),
                    "{case}"
                );
                let scope = Request {
                    id: 0,
                    cancellation: Cancellation::new().unwrap(),
                };
                assert_eq!(
                    drive(&files, e.load_identity(&scope)).unwrap().is_some(),
                    matches!(case, "sync" | "cancel-rename")
                );
            }
            assert_eq!(files.reactor.in_flight(), 0);
            if matches!(case, "sync" | "abandon" | "cancel-rename") {
                sim.disk().crash().unwrap();
                assert!(sim.metadata(Path::new("/private/identity.json")).is_ok());
                assert!(sim.metadata(Path::new("/private/pending.json")).is_err());
            }
        }
    }

    /// Overlap cannot steal serialization; cancellation before admission causes no I/O.
    #[test]
    fn attempts_reject_overlap_and_fence_abandoned_work_before_fresh_scope() {
        let sim = Simulation::new();
        let _environment = sim.enter();
        let (files, e, scope) = fixture();
        sim.inject("open", Fault::Delay(10)).unwrap();
        let mut first = e.prepare(vec![], NonZeroU32::new(4).unwrap(), &scope);
        assert!(
            first
                .as_mut()
                .poll(&mut Context::from_waker(futures::task::noop_waker_ref()))
                .is_pending()
        );
        assert_eq!(files.next.get(), 1);
        assert!(
            matches!(drive(&files, e.prepare(vec![], NonZeroU32::new(4).unwrap(), &scope)), Err(error) if error == uring_runtime::Error::Overloaded.into())
        );
        assert_eq!(files.next.get(), 1);
        drop(first);
        assert!(files.reactor.in_flight() > 0);
        drive(
            &files,
            e.prepare(vec![], NonZeroU32::new(4).unwrap(), &scope),
        )
        .unwrap();
        assert_eq!(files.next.get(), 2);
        assert_eq!(files.reactor.in_flight(), 0);
        scope.cancellation.cancel().unwrap();
        let before = sim.trace().len();
        assert!(
            matches!(drive(&files, e.load_identity(&scope)), Err(error) if error == uring_runtime::Error::Cancelled.into())
        );
        assert_eq!(sim.trace().len(), before);
        assert_eq!(files.next.get(), 2);
    }

    /// Lifetime-based renewal and recovery treat expired authentication as absent.
    #[test]
    fn renewal_and_expired_recovery_follow_scoped_time() {
        let sim = Simulation::new();
        let _environment = sim.enter();
        let clock = uring_runtime::environment::SimulationClock::new(99);
        let _time = clock.environment(0).enter();
        let (files, e, scope) = fixture();
        let request = drive(
            &files,
            e.prepare(vec![], NonZeroU32::new(4).unwrap(), &scope),
        )
        .unwrap();
        let (ca, key) = identity::test_util::ca();
        e.set_peer_trust_roots(vec![ca.der().to_vec()]).unwrap();
        let identity = drive(
            &files,
            e.accept_response(issue(&request, &ca, &key, |_| {}), &scope),
        )
        .unwrap();
        let start = UNIX_EPOCH + Duration::from_secs(identity.not_before);
        let lifetime = identity.not_after - identity.not_before;
        clock.set_wall_time(start + Duration::from_secs(lifetime * 2 / 3 - 1));
        assert!(!identity.renewal_due());
        clock.set_wall_time(start + Duration::from_secs(lifetime * 2 / 3 + 1));
        assert!(identity.renewal_due());
        assert!(identity.valid_now());
        clock.set_wall_time(identity.expires_at() + Duration::from_secs(1));
        assert!(!identity.valid_now());
        assert!(drive(&files, e.load_identity(&scope)).unwrap().is_none());
        clock.set_wall_time(start - Duration::from_secs(1));
        assert!(!identity.valid_now());
    }

    /// Shared secure helpers keep symlink policy, permissions and size errors intact.
    #[test]
    fn secure_helpers_reject_private_links_modes_and_oversized_reads() {
        let sim = Simulation::new();
        let _environment = sim.enter();
        let (files, e, scope) = fixture();
        drive(
            &files,
            e.prepare(vec![], NonZeroU32::new(4).unwrap(), &scope),
        )
        .unwrap();
        sim.chmod(Path::new("/private"), 0o755).unwrap();
        assert!(
            matches!(drive(&files, e.load_identity(&scope)), Err(error) if error == secure::AccessError::PermissionDenied.into())
        );
        sim.chmod(Path::new("/private"), 0o700).unwrap();
        sim.write_file(Path::new("/data"), b"token").unwrap();
        sim.symlink(Path::new("/data"), Path::new("/private/link"))
            .unwrap();
        let dir = drive(
            &files,
            Box::pin(secure::directory(
                files.reactor(),
                Path::new("/private"),
                false,
                true,
                &scope,
            )),
        )
        .unwrap();
        for (name, limit, private, expected) in [
            ("link", 5, false, uring_runtime::Error::Os(40).into()),
            (
                "pending.json",
                1024 * 1024 + 1,
                true,
                uring_runtime::Error::Overloaded.into(),
            ),
            (
                "pending.json",
                1,
                true,
                uring_runtime::Error::InvalidInput.into(),
            ),
        ] {
            assert!(
                matches!(drive(&files, Box::pin(secure::read_at(files.reactor(), &dir, name, limit, private, &scope))), Err(actual) if actual == expected)
            );
        }
        let oversized = vec![0; 1024 * 1024 + 1];
        let result = drive(
            &files,
            Box::pin(async {
                secure::atomic_write(files.reactor(), &dir, "large", &oversized, &scope)
                    .await
                    .map_err(Failure::from)
            }),
        );
        assert_eq!(result, Err(uring_runtime::Error::Overloaded.into()));
        assert!(sim.metadata(Path::new("/private/.large.stage")).is_err());
    }
}
