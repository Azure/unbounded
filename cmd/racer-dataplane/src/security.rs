//! Vetted XChaCha20-Poly1305 adapter, canonical page AAD, fresh cryptographic nonces.

use crate::admission::AdmissionPolicy;
use crate::admission::ResourceClass;
use crate::error::Error;
use crate::error::Operation;
use crate::error::Result;
use crate::memory::CiphertextBytes;
use crate::memory::CiphertextPage;
use crate::memory::PlaintextBuffer;
use crate::memory::VerifiedBytes;
use crate::memory::VerifiedPage;
use crate::model::AttemptId;
use crate::model::MAX_FIELD_BYTES;
use crate::model::Nonce;
use crate::model::ObjectId;
use crate::model::PageEnvelope;
use crate::model::PageId;
use crate::model::RequestId;
use crate::model::WorkerId;
use crate::runtime::RequestScope;
use crate::telemetry::AeadFailure;
use crate::telemetry::Failure;
use crate::telemetry::Stage;
use racer_control_wire::KeyId;
use racer_crypto::TAG_LEN;
use racer_crypto::identity::KeyLease;
use racer_crypto::identity::KeyPurpose;
use racer_crypto::identity::Keyring;
use std::cell::RefCell;
use std::fmt;
use std::num::NonZeroUsize;
use std::ops::Deref;
use std::rc::Rc;
use std::sync::Arc;
#[cfg(test)]
use std::sync::atomic::Ordering;
use std::task::Context;
use std::task::Poll;
use std::task::Waker;
use uring_runtime::offload;
use uring_runtime::reactor::IoBuffer;
use zeroize::Zeroizing;

impl From<racer_crypto::identity::Error> for crate::error::Error {
    fn from(error: racer_crypto::identity::Error) -> Self {
        match error {
            racer_crypto::identity::Error::InvalidRequest => Self::InvalidRequest,
            racer_crypto::identity::Error::InvalidConfiguration => Self::InvalidConfiguration,
            racer_crypto::identity::Error::Unauthorized => Self::Unauthorized,
            racer_crypto::identity::Error::Unavailable => Self::Unavailable,
            racer_crypto::identity::Error::MissingKey => Self::MissingKey,
            racer_crypto::identity::Error::CorruptRecord => Self::CorruptRecord,
        }
    }
}

// Request-scoped adapter context, deliberately absent from cache identities.
// Credentials travel encrypted across peers and as local origin headers, never
// in caches, checkpoints, logs, metrics, or durable retry queues.
pub const METADATA_HEADER: &str = "Racer-Metadata";
/// Sensitive bytes with redacted diagnostics and zeroization on drop. Not Clone.
pub struct Authorization {
    bytes: Zeroizing<Vec<u8>>,
}
impl Authorization {
    /// Validate HTTP field syntax/size without interpreting the credential scheme.
    pub fn from_header(bytes: &[u8]) -> Result<Self> {
        validate_opaque(bytes)?;
        Ok(Self {
            bytes: Zeroizing::new(bytes.to_vec()),
        })
    }
    /// Expose only for encryption or local adapter writes, never diagnostics.
    pub fn expose_for_origin(&self) -> &[u8] {
        self.bytes.as_slice()
    }
}
/// Preserve one bounded field value. The HTTP parser must reject duplicate fields.
pub struct OpaqueMetadata {
    bytes: Zeroizing<Vec<u8>>,
}
impl OpaqueMetadata {
    pub fn from_header(bytes: &[u8]) -> Result<Self> {
        validate_opaque(bytes)?;
        Ok(Self {
            bytes: Zeroizing::new(bytes.to_vec()),
        })
    }
    pub fn as_header(&self) -> &[u8] {
        self.bytes.as_slice()
    }
}
fn validate_opaque(bytes: &[u8]) -> Result<()> {
    if bytes.is_empty()
        || bytes.len() > MAX_FIELD_BYTES
        || bytes.first() == Some(&b' ')
        || bytes.last() == Some(&b' ')
        || bytes.iter().any(|&byte| byte < 0x20 || byte == 0x7f)
    {
        return Err(Error::InvalidRequest);
    }
    Ok(())
}
impl fmt::Debug for Authorization {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str("Authorization([redacted])")
    }
}
impl fmt::Debug for OpaqueMetadata {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str("OpaqueMetadata([redacted])")
    }
}
/// One request owns the raw context and lends it to origin writes and sealing.
/// Retrying/fanning out must not require duplicating secrets:
/// ```compile_fail
/// use racer_dataplane::security::OriginContext;
/// fn duplicate(origin: OriginContext) { let _copy = origin.clone(); }
/// ```
pub struct OriginContext {
    pub object: ObjectId,
    pub metadata: Option<OpaqueMetadata>,
    pub authorization: Option<Authorization>,
}
impl fmt::Debug for OriginContext {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str("OriginContext([redacted])")
    }
}
/// Separate AEAD domain from pages. Bind request/attempt/object/metadata with
/// canonical AAD; retries reseal with fresh nonces rather than change bound fields.
/// Eligible origin-fetching nodes can open this cache-scoped credential envelope.
pub struct EncryptedAuthorization {
    pub key_id: KeyId,
    pub nonce: Nonce,
    pub ciphertext: Vec<u8>,
}
impl fmt::Debug for EncryptedAuthorization {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str("EncryptedAuthorization([redacted])")
    }
}
/// Owned per-attempt envelope, with no borrow of raw context. Relays preserve it
/// unopened. Local quota and scope are never serialized on the wire.
/// ```compile_fail
/// use racer_dataplane::security::PeerOriginContext;
/// fn duplicate(envelope: PeerOriginContext) { let _copy = envelope.clone(); }
/// ```
/// Wire decoding must admit allocations before constructing this owner:
/// ```compile_fail
/// use racer_dataplane::{security::PeerOriginContext, model::{ObjectId, RequestId, AttemptId}};
/// fn uncharged(object: ObjectId, request: RequestId, attempt: AttemptId) {
///     let _envelope = PeerOriginContext { object, request, attempt, metadata: None, authorization: None };
/// }
/// ```
pub struct PeerOriginContext {
    pub object: ObjectId,
    pub request: RequestId,
    pub attempt: AttemptId,
    pub metadata: Option<OpaqueMetadata>,
    pub authorization: Option<EncryptedAuthorization>,
    /// Retain field allocation charges until transport I/O is fenced.
    pub(crate) reservation: flow_control::Charge<AdmissionPolicy>,
    pub(crate) scope: RequestScope,
}
impl PeerOriginContext {
    /// Original deadline/shared cancellation, never reset by retry or fanout.
    pub fn scope(&self) -> &RequestScope {
        &self.scope
    }
}

pub(crate) fn fresh_nonce() -> Result<Nonce> {
    let mut nonce = [0u8; 24];
    uring_runtime::environment::fill_random(&mut nonce).map_err(|_| Error::Unavailable)?;
    Ok(Nonce(nonce))
}
pub(crate) fn field(out: &mut Vec<u8>, value: &[u8]) -> Result<()> {
    let length = u32::try_from(value.len()).map_err(|_| Error::InvalidRequest)?;
    out.extend_from_slice(&length.to_be_bytes());
    out.extend_from_slice(value);
    Ok(())
}
/// v1: domain, cache(length:u32 BE, bytes), exact object key, quoted ETag
/// (length:u32 BE, bytes), page:u64 BE, key ID, nonce, lengths:u32 BE.
pub fn page_aad(envelope: &PageEnvelope) -> Result<Vec<u8>> {
    envelope.validate()?;
    if !racer_crypto::identity::canonical_uuid(&envelope.page.version.object.cache.0)
        || envelope.page.version.etag.as_bytes().len() > crate::model::MAX_FIELD_BYTES
    {
        return Err(Error::CorruptRecord);
    }
    let mut out = b"racer/page/aead/v1\0".to_vec();
    field(&mut out, envelope.page.version.object.cache.0.as_bytes())?;
    out.extend_from_slice(&envelope.page.version.object.key.0);
    field(&mut out, envelope.page.version.etag.as_bytes())?;
    out.extend_from_slice(&envelope.page.number.0.to_be_bytes());
    out.extend_from_slice(&envelope.key_id.0);
    out.extend_from_slice(&envelope.nonce.0);
    out.extend_from_slice(&envelope.plaintext_length.to_be_bytes());
    out.extend_from_slice(&envelope.ciphertext_length.to_be_bytes());
    Ok(out)
}
/// Called only on crypto for exact AEAD rejects or opt-in send samples. Reuses
/// the CRC already initialized by verify_checksum; never scans payload bytes.
pub(crate) fn capture_aead_failure(
    ciphertext: &CiphertextPage,
    aad: &[u8],
    request: RequestId,
) -> AeadFailure {
    use sha2::Digest;
    use sha2::Sha256;
    let envelope = ciphertext.envelope();
    let mut hash = Sha256::new();
    hash.update(b"racer/diagnostic/page/v1\0");
    for field in [
        envelope.page.version.object.cache.0.as_bytes(),
        &envelope.page.version.object.key.0,
        envelope.page.version.etag.as_bytes(),
        &envelope.page.number.0.to_be_bytes(),
    ] {
        hash.update((field.len() as u64).to_be_bytes());
        hash.update(field);
    }
    AeadFailure {
        unix_millis: Failure::new(Stage::PeerDecode, Error::CorruptRecord).unix_millis,
        request,
        peer: ciphertext.provenance,
        page: hash.finalize().into(),
        number: envelope.page.number.0,
        key: envelope.key_id.0,
        nonce: envelope.nonce.0,
        plaintext: envelope.plaintext_length,
        ciphertext: envelope.ciphertext_length,
        aad: Sha256::digest(aad).into(),
        crc: ciphertext.cached_checksum(),
    }
}

pub struct PageCrypto {
    keys: Rc<Keyring>,
    client: Rc<CryptoClient>,
}
impl PageCrypto {
    pub(crate) async fn sample_send(
        &self,
        ciphertext: CiphertextPage,
        scope: &RequestScope,
        sample: crate::telemetry::Work,
    ) {
        let envelope = ciphertext.envelope();
        let key = match self.keys.lease(
            Some(&envelope.page.version.object.cache),
            envelope.key_id,
            KeyPurpose::Page,
        ) {
            Ok(key) => key,
            Err(error) => {
                sample.finish(Some(error.into()));
                return;
            }
        };
        let _ = self
            .client
            .execute_sample(
                CryptoInput::Checksum { ciphertext },
                key,
                scope,
                Some(sample),
            )
            .await;
    }
    /// Verify persisted ciphertext integrity on the bounded crypto worker queue.
    /// This does not authenticate a peer copy or produce publishable plaintext.
    pub fn verify_checksum<'a>(
        &'a self,
        ciphertext: CiphertextPage,
        scope: &'a RequestScope,
    ) -> Operation<'a, ()> {
        Box::pin(async move {
            let envelope = ciphertext.envelope();
            let key = self.keys.lease(
                Some(&envelope.page.version.object.cache),
                envelope.key_id,
                KeyPurpose::Page,
            )?;
            match self
                .client
                .execute(CryptoInput::Checksum { ciphertext }, key, scope)
                .await?
            {
                CryptoOutput::Checksummed(_) => Ok(()),
                _ => Err(Error::CorruptRecord),
            }
        })
    }
    /// I/O-local facade. Only owned page inputs and an immutable key lease cross
    /// to the engine; this Rc graph and its futures stay on I/O.
    pub fn new(keys: Rc<Keyring>, client: Rc<CryptoClient>) -> Self {
        Self { keys, client }
    }
    /// Authenticate the entire page before constructing a publishable lease.
    /// Futures stay on I/O, even though inputs/completions are Send:
    /// ```compile_fail
    /// use racer_dataplane::{security::PageCrypto,
    ///     memory::CiphertextPage,
    ///     admission::AdmissionPolicy, runtime::RequestScope};
    /// fn require_send<T: Send>(_: T) {}
    /// fn move_future(crypto: &PageCrypto, page: CiphertextPage,
    ///     output: flow_control::Charge<AdmissionPolicy>, scope: &RequestScope) {
    ///     require_send(crypto.decrypt(page, output, scope));
    /// }
    /// ```
    pub fn decrypt<'a>(
        &'a self,
        ciphertext: CiphertextPage,
        plaintext: flow_control::Charge<AdmissionPolicy>,
        scope: &'a RequestScope,
    ) -> Operation<'a, VerifiedPage> {
        Box::pin(async move {
            let envelope = ciphertext.envelope();
            let key = self.keys.lease(
                Some(&envelope.page.version.object.cache),
                envelope.key_id,
                KeyPurpose::Page,
            )?;
            match self
                .client
                .execute(
                    CryptoInput::Decrypt {
                        ciphertext,
                        plaintext,
                    },
                    key,
                    scope,
                )
                .await?
            {
                CryptoOutput::Decrypted(page, _original_ciphertext) => Ok(page),
                CryptoOutput::Encrypted(..) | CryptoOutput::Checksummed(_) => {
                    Err(Error::CorruptRecord)
                }
            }
        })
    }
    /// Encrypt once; original ciphertext is reused verbatim for peers and disk.
    pub fn encrypt<'a>(
        &'a self,
        page: PageId,
        plaintext: PlaintextBuffer,
        ciphertext: flow_control::Charge<AdmissionPolicy>,
        scope: &'a RequestScope,
    ) -> Operation<'a, (VerifiedPage, CiphertextPage)> {
        Box::pin(async move {
            let key = self
                .keys
                .active(&page.version.object.cache, KeyPurpose::Page)?;
            match self
                .client
                .execute(
                    CryptoInput::Encrypt {
                        page,
                        plaintext,
                        ciphertext,
                    },
                    key,
                    scope,
                )
                .await?
            {
                CryptoOutput::Encrypted(plaintext, ciphertext) => Ok((plaintext, ciphertext)),
                CryptoOutput::Decrypted(..) | CryptoOutput::Checksummed(_) => {
                    Err(Error::CorruptRecord)
                }
            }
        })
    }
}

/// Constructed on the paired crypto thread, without access to the I/O service graph.
/// The vetted AEAD adapter processes owned jobs in bounded quanta, checks the
/// original deadline/cancellation, and returns every accepted job as a completion.
pub struct PageCryptoEngine {
    environment: uring_runtime::environment::Environment,

    runtime: CryptoRuntime,

    executor: offload::Worker<CryptoCompletion>,
}
impl PageCryptoEngine {
    pub fn new(runtime: CryptoRuntime) -> Self {
        Self {
            environment: uring_runtime::environment::Environment::current(),
            runtime,
            executor: offload::Worker::default(),
        }
    }
    /// All fallible work borrows input. Ownership transfers only after the final
    /// cancellation check, so failures retain the exact input and quota owners.
    pub fn process(job: CryptoJob) -> CryptoCompletion {
        let measurement_start = job.permit.execution_start();
        let CryptoJob {
            mut permit,
            input,
            key,
            scope,
        } = job;
        let prepared = Self::prepare(&input, &key, &scope, &mut permit);
        if let Some(sample) = &mut permit.send_sample
            && let CryptoInput::Checksum { ciphertext } = &input
            && let Ok(aad) = page_aad(ciphertext.envelope())
        {
            sample.facts = Some(capture_aead_failure(ciphertext, &aad, scope.request));
        }
        let outcome = match prepared {
            Err(error) => CryptoOutcome::Failed { input, error },
            Ok((envelope, bytes)) => Self::complete(input, envelope, bytes),
        };
        if let Some(start) = measurement_start {
            permit.executed(start);
        }
        CryptoCompletion {
            permit,
            outcome,
            _key: key,
        }
    }

    /// Borrow every quota owner until crypto and the final cancellation check succeed.
    fn prepare(
        input: &CryptoInput,
        key: &racer_crypto::identity::KeyLease,
        scope: &RequestScope,
        permit: &mut CryptoPermit,
    ) -> Result<(PageEnvelope, Zeroizing<Vec<u8>>)> {
        scope.check()?;
        if let CryptoInput::Checksum { ciphertext } = input {
            if let Some(sample) = &mut permit.send_sample {
                sample.cached = ciphertext.cached_checksum().is_some();
                if sample.cached {
                    return Ok((ciphertext.envelope().clone(), Zeroizing::new(Vec::new())));
                }
            }
            ciphertext.verify_checksum()?;
            scope.check()?;
            return Ok((ciphertext.envelope().clone(), Zeroizing::new(Vec::new())));
        }
        key.require_purpose(KeyPurpose::Page)?;
        let (envelope, mut bytes) = match input {
            CryptoInput::Checksum { .. } => unreachable!("checksum handled above"),
            CryptoInput::Encrypt {
                page,
                plaintext,
                ciphertext,
            } => {
                if key.cache() != &page.version.object.cache {
                    return Err(Error::MissingKey);
                }
                let raw = plaintext.bytes()?;
                plaintext
                    .reservation()
                    .validate(ResourceClass::Plaintext, raw.len())?;
                if plaintext.reservation().key() != Some(&page.version.object.cache) {
                    return Err(Error::InvalidConfiguration);
                }
                let length = u32::try_from(raw.len()).map_err(|_| Error::InvalidRequest)?;
                let envelope = PageEnvelope {
                    page: page.clone(),
                    key_id: key.id(),
                    nonce: fresh_nonce()?,
                    plaintext_length: length,
                    ciphertext_length: length.checked_add(16).ok_or(Error::InvalidRequest)?,
                };
                let aad = page_aad(&envelope)?;
                ciphertext.validate(
                    ResourceClass::Ciphertext,
                    envelope.ciphertext_length as usize,
                )?;
                if ciphertext.key() != Some(&page.version.object.cache) {
                    return Err(Error::InvalidConfiguration);
                }
                let mut bytes =
                    Zeroizing::new(ciphertext.buffer(envelope.ciphertext_length as usize)?);
                // Input remains immutable until the final cancellation check.
                // Admission supplies initialized output, including the tag tail.
                key.seal_page(
                    &page.version.object.cache,
                    &envelope.nonce.0,
                    &aad,
                    raw,
                    &mut bytes,
                )?;
                (envelope, bytes)
            }
            CryptoInput::Decrypt {
                ciphertext,
                plaintext,
            } => {
                let envelope = ciphertext.envelope();
                ciphertext.verify_checksum().inspect_err(|_| {
                    permit.rejected(IntegrityRejection::Crc);
                })?;
                if key.cache() != &envelope.page.version.object.cache || key.id() != envelope.key_id
                {
                    return Err(Error::MissingKey);
                }
                let aad = page_aad(envelope)?;
                let length = envelope.plaintext_length as usize;
                if ciphertext.bytes().len() != envelope.ciphertext_length as usize
                    || length.checked_add(TAG_LEN) != Some(ciphertext.bytes().len())
                {
                    return Err(Error::CorruptRecord);
                }
                plaintext.validate(ResourceClass::Plaintext, envelope.plaintext_length as usize)?;
                if plaintext.key() != Some(&envelope.page.version.object.cache) {
                    return Err(Error::InvalidConfiguration);
                }
                // Decryption allocates only plaintext-length storage,
                // never an uncharged tag-sized tail under plaintext admission.
                let mut bytes = Zeroizing::new(plaintext.buffer(length)?);
                key.open_page(
                    &envelope.page.version.object.cache,
                    envelope.key_id,
                    &envelope.nonce.0,
                    &aad,
                    ciphertext.bytes(),
                    &mut bytes,
                )
                .map_err(|_| {
                    permit.rejected(IntegrityRejection::Aead);
                    permit.aead_failure =
                        Some(capture_aead_failure(ciphertext, &aad, scope.request));
                    Error::CorruptRecord
                })?;
                (envelope.clone(), bytes)
            }
        };
        scope.check()?;
        Ok((envelope, std::mem::take(&mut bytes)))
    }

    /// Infallible ownership transfer after preparation; failures never consume input.
    fn complete(
        input: CryptoInput,
        envelope: PageEnvelope,
        mut bytes: Zeroizing<Vec<u8>>,
    ) -> CryptoOutcome {
        match input {
            CryptoInput::Checksum { ciphertext } => {
                CryptoOutcome::Completed(CryptoOutput::Checksummed(ciphertext))
            }
            CryptoInput::Encrypt {
                page,
                plaintext,
                mut ciphertext,
            } => {
                let (plain, reservation) = plaintext.into_parts();
                ciphertext
                    .shrink(bytes.capacity())
                    .expect("validated crypto capacity");
                let plain = VerifiedPage {
                    inner: Arc::new(VerifiedBytes {
                        page,
                        bytes: plain.into_vec(),
                        reservation,
                    }),
                };
                let encrypted = CiphertextPage {
                    provenance: None,
                    inner: Arc::new(CiphertextBytes {
                        checksum: std::sync::OnceLock::from(racer_crypto::crc64(&bytes)),
                        envelope,
                        bytes: std::mem::take(&mut *bytes),
                        reservation: ciphertext,
                    }),
                };
                CryptoOutcome::Completed(CryptoOutput::Encrypted(plain, encrypted))
            }
            CryptoInput::Decrypt {
                ciphertext,
                mut plaintext,
            } => {
                plaintext
                    .shrink(bytes.capacity())
                    .expect("validated crypto capacity");
                let page = VerifiedPage {
                    inner: Arc::new(VerifiedBytes {
                        page: envelope.page,
                        bytes: std::mem::take(&mut *bytes),
                        reservation: plaintext,
                    }),
                };
                CryptoOutcome::Completed(CryptoOutput::Decrypted(page, ciphertext))
            }
        }
    }
    fn drive(&mut self, cx: &mut Context<'_>, budget: usize) -> Result<()> {
        let _environment = self.environment.enter();
        self.executor
            .poll(&mut self.runtime.port.queue, cx, budget, |mut job| {
                job.permit.measurement.queue_ns = job.permit.measurement.submitted.map(elapsed_ns);
                Self::process(job)
            })
            .map_err(Into::into)
    }
}
impl uring_runtime::group::Service<RequestScope> for PageCryptoEngine {
    fn register_driver(&self, waker: &std::task::Waker) {
        self.runtime.port.register_driver(waker);
    }
    fn start<'a>(&'a mut self, scope: &'a RequestScope) -> Operation<'a, ()> {
        Box::pin(self.environment.scope(async move { scope.check() }))
    }
    fn poll_budgeted(&mut self, cx: &mut Context<'_>, work_budget: usize) -> Result<()> {
        self.drive(cx, work_budget)
    }
    fn drain<'a>(&'a mut self, scope: &'a RequestScope) -> Operation<'a, ()> {
        Box::pin(futures::future::poll_fn(move |cx| {
            // Deadline never drops accepted owners; canceled jobs become failures.
            // Match the shared executor's one-page turn during teardown too.
            if let Err(error) = self.drive(cx, 1) {
                return Poll::Ready(Err(error));
            }
            if self.executor.is_drained() {
                Poll::Ready(Ok(()))
            } else {
                let _ = scope;
                Poll::Pending
            }
        }))
    }
    fn shutdown<'a>(&'a mut self, scope: &'a RequestScope) -> Operation<'a, ()> {
        self.drain(scope)
    }
}
/// Decrypted context remains charged for the complete local origin operation.
pub struct ChargedOriginContext {
    context: OriginContext,
    _reservation: flow_control::Charge<AdmissionPolicy>,
}
impl Deref for ChargedOriginContext {
    type Target = OriginContext;
    fn deref(&self) -> &OriginContext {
        &self.context
    }
}

fn credential_aad(
    key: KeyId,
    object: &ObjectId,
    request: RequestId,
    attempt: AttemptId,
    metadata: Option<&[u8]>,
) -> Result<Zeroizing<Vec<u8>>> {
    // Allocate once so reallocations cannot leave metadata in an unwiped buffer.
    let capacity = 128usize
        .checked_add(object.cache.0.len())
        .and_then(|n| n.checked_add(metadata.map_or(0, <[u8]>::len)))
        .ok_or(Error::InvalidRequest)?;
    let mut out = Zeroizing::new(Vec::with_capacity(capacity));
    out.extend_from_slice(b"racer/credentials/aead/v1\0");
    out.extend_from_slice(&key.0);
    field(&mut out, object.cache.0.as_bytes())?;
    out.extend_from_slice(&object.key.0);
    out.extend_from_slice(&request.0);
    out.extend_from_slice(&attempt.0);
    out.push(u8::from(metadata.is_some()));
    if let Some(metadata) = metadata {
        field(&mut out, metadata)?;
    }
    Ok(out)
}
fn credential_bounds(
    object: &ObjectId,
    metadata: Option<&[u8]>,
    credential_length: usize,
) -> Result<usize> {
    if !racer_crypto::identity::canonical_uuid(&object.cache.0)
        || metadata.is_some_and(|m| m.len() > crate::model::MAX_FIELD_BYTES)
        || credential_length > crate::model::MAX_FIELD_BYTES + 16
    {
        return Err(Error::InvalidRequest);
    }
    512usize
        .checked_add(object.cache.0.len())
        .and_then(|n| n.checked_add(metadata.map_or(0, <[u8]>::len)))
        .and_then(|n| n.checked_add(credential_length))
        .ok_or(Error::InvalidRequest)
}
/// Ephemeral credential AEAD, separate from immutable page encryption and storage.
/// Domain-separate keys/AAD from pages. Bind cache/key, request/attempt, and opaque
/// metadata. Relays keep this envelope opaque; eligible origin fetchers may open it.
/// No credential fingerprint or credential value becomes a cache/singleflight key.
/// One request owner lends its raw context to each seal; independently owned
/// envelopes can coexist and leave that borrow's lifetime. Nonces are generated
/// internally and temporary secrets are zeroized.
/// ```no_run
/// use racer_dataplane::{error::Result, model::AttemptId,
///     runtime::RequestScope, security::{CredentialCrypto, OriginContext, PeerOriginContext}};
/// fn fanout(crypto: &CredentialCrypto, origin: &OriginContext, scope: &RequestScope,
///     first: AttemptId, second: AttemptId) -> Result<(PeerOriginContext, PeerOriginContext)> {
///     let a = crypto.seal(origin, first, scope)?;
///     let b = crypto.seal(origin, second, scope)?;
///     Ok((a, b))
/// }
/// fn retry_after_failure(crypto: &CredentialCrypto, origin: &OriginContext,
///     scope: &RequestScope, first: AttemptId, retry: AttemptId) -> Result<PeerOriginContext> {
///     match crypto.seal(origin, first, scope) {
///         Ok(envelope) => Ok(envelope),
///         Err(_) => crypto.seal(origin, retry, scope),
///     }
/// }
/// ```
pub struct CredentialCrypto {
    keys: Rc<Keyring>,
    admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
}
impl CredentialCrypto {
    /// Independently admitted same-worker context. No trust boundary is crossed;
    /// retain exact fields and zeroization without a credential AEAD round trip.
    pub(crate) fn local_context(
        &self,
        context: &OriginContext,
        scope: &RequestScope,
    ) -> Result<ChargedOriginContext> {
        scope.check()?;
        let metadata = context.metadata.as_ref().map(OpaqueMetadata::as_header);
        let raw = context
            .authorization
            .as_ref()
            .map(Authorization::expose_for_origin);
        let size = credential_bounds(&context.object, metadata, raw.map_or(0, <[u8]>::len))?;
        let reservation = self.admission.reserve(
            Some(&context.object.cache),
            ResourceClass::RequestContext,
            size,
        )?;
        let context = OriginContext {
            object: context.object.clone(),
            metadata: metadata.map(OpaqueMetadata::from_header).transpose()?,
            authorization: raw.map(Authorization::from_header).transpose()?,
        };
        scope.check()?;
        Ok(ChargedOriginContext {
            context,
            _reservation: reservation,
        })
    }
    pub fn new(keys: Rc<Keyring>, admission: Rc<flow_control::Quotas<AdmissionPolicy>>) -> Self {
        Self { keys, admission }
    }
    /// Borrow only for this bounded synchronous call; never retain, clone, enqueue,
    /// or cache the raw context. Each retry/fanout uses a new attempt ID and calls
    /// seal again, even when the credential bytes are identical. The output owns
    /// its object/metadata copies and encrypted bytes, independent of this borrow.
    ///
    /// Before allocation, check the original scope and all field/encoded lengths
    /// (including overflow, tag, and AAD scratch), then reserve RequestContext bytes
    /// against context.object.cache. Retain output quota in PeerOriginContext;
    /// temporary quota ends only when sealing stops accessing its scratch. Reject
    /// saturation rather than allocating uncharged bytes. Copy opaque metadata
    /// exactly, preserving absent versus present-empty; never normalize headers.
    ///
    /// Select KeyPurpose::OriginCredentials, never a page key. For every encryption
    /// generate a fresh CSPRNG XChaCha20 nonce internally, independent of attempt IDs,
    /// page nonces, and connection challenges. Canonical, versioned credential-domain
    /// AAD binds key ID, object (cache UID + exact key), scope.request, attempt, and
    /// opaque metadata presence/length/bytes. Encrypt the exact Authorization bytes.
    /// A missing Authorization still requires an owned, charged peer context and
    /// signed metadata; it must not bypass scope checks or admission.
    ///
    /// Recheck cancellation/deadline before returning; errors release local work
    /// without consuming origin. No detached work can outlive this borrow. The
    /// envelope retains the original scope for transport, which must retain quota
    /// through any submitted I/O completion even if its waiting future is dropped.
    /// Output and temporary scratch have independent admission charges.
    pub fn seal(
        &self,
        context: &OriginContext,
        attempt: AttemptId,
        scope: &RequestScope,
    ) -> Result<PeerOriginContext> {
        scope.check()?;
        let metadata = context.metadata.as_ref().map(OpaqueMetadata::as_header);
        let raw = context
            .authorization
            .as_ref()
            .map(Authorization::expose_for_origin);
        let size = credential_bounds(&context.object, metadata, raw.map_or(0, <[u8]>::len) + 16)?;
        let reservation = self.admission.reserve(
            Some(&context.object.cache),
            ResourceClass::RequestContext,
            size,
        )?;
        let _scratch = self.admission.reserve(
            Some(&context.object.cache),
            ResourceClass::RequestContext,
            size,
        )?;
        let authorization = if let Some(raw) = raw {
            let key = self
                .keys
                .active(&context.object.cache, KeyPurpose::OriginCredentials)?;
            let nonce = fresh_nonce()?;
            let aad = credential_aad(key.id(), &context.object, scope.request, attempt, metadata)?;
            let mut bytes = Zeroizing::new(vec![0; raw.len() + TAG_LEN]);
            key.seal_credentials(&context.object.cache, &nonce.0, &aad, raw, &mut bytes)
                .map_err(Error::from)?;
            Some(EncryptedAuthorization {
                key_id: key.id(),
                nonce,
                ciphertext: std::mem::take(&mut *bytes),
            })
        } else {
            None
        };
        let metadata = metadata.map(OpaqueMetadata::from_header).transpose()?;
        scope.check()?;
        Ok(PeerOriginContext {
            object: context.object.clone(),
            request: scope.request,
            attempt,
            metadata,
            authorization,
            reservation,
            scope: scope.clone(),
        })
    }
    pub fn open_charged(
        &self,
        context: PeerOriginContext,
        request: RequestId,
        attempt: AttemptId,
    ) -> Result<ChargedOriginContext> {
        context.scope.check()?;
        if context.request != request
            || context.attempt != attempt
            || context.scope.request != request
        {
            return Err(Error::Unauthorized);
        }
        let metadata = context.metadata.as_ref().map(OpaqueMetadata::as_header);
        let size = credential_bounds(
            &context.object,
            metadata,
            context
                .authorization
                .as_ref()
                .map_or(0, |a| a.ciphertext.len()),
        )?;
        context
            .reservation
            .validate(ResourceClass::RequestContext, size)?;
        if context.reservation.key() != Some(&context.object.cache) {
            return Err(Error::Unauthorized);
        }
        let output = self.admission.reserve(
            Some(&context.object.cache),
            ResourceClass::RequestContext,
            size,
        )?;
        let _scratch = self.admission.reserve(
            Some(&context.object.cache),
            ResourceClass::RequestContext,
            size.checked_mul(2).ok_or(Error::InvalidRequest)?,
        )?;
        let authorization = if let Some(encrypted) = &context.authorization {
            if encrypted.ciphertext.len() < 16 {
                return Err(Error::Unauthorized);
            }
            let key = self.keys.lease(
                Some(&context.object.cache),
                encrypted.key_id,
                KeyPurpose::OriginCredentials,
            )?;
            let aad = credential_aad(key.id(), &context.object, request, attempt, metadata)?;
            let mut bytes = Zeroizing::new(vec![0; encrypted.ciphertext.len() - TAG_LEN]);
            key.open_credentials(
                &context.object.cache,
                encrypted.key_id,
                &encrypted.nonce.0,
                &aad,
                &encrypted.ciphertext,
                &mut bytes,
            )
            .map_err(Error::from)?;
            Some(Authorization::from_header(&bytes)?)
        } else {
            None
        };
        context.scope.check()?;
        Ok(ChargedOriginContext {
            context: OriginContext {
                object: context.object,
                metadata: context.metadata,
                authorization,
            },
            _reservation: output,
        })
    }
}

/// Rejected crypto work retains its owner and Racer policy failure.
pub struct CryptoSendFailure<T> {
    pub command: T,
    pub error: Error,
}
impl<T> From<uring_runtime::channel::SendFailure<T>> for CryptoSendFailure<T> {
    fn from(failure: uring_runtime::channel::SendFailure<T>) -> Self {
        Self {
            command: failure.command,
            error: failure.error.into(),
        }
    }
}
impl From<offload::Error> for Error {
    fn from(error: offload::Error) -> Self {
        match error {
            offload::Error::Stale => Self::StaleFlight,
            offload::Error::Runtime(error) => error.into(),
        }
    }
}
impl<T> From<offload::SendFailure<T>> for CryptoSendFailure<T> {
    fn from(failure: offload::SendFailure<T>) -> Self {
        Self {
            command: failure.command,
            error: failure.error.into(),
        }
    }
}
/// I/O-generated identity, independent of flights. Never reuse a sequence within
/// a pair generation; restart increments the generation and rejects late results.
#[derive(Clone, Copy, Debug, Eq, Hash, PartialEq)]
pub struct CryptoId {
    pub worker: WorkerId,
    pub generation: u64,
    pub sequence: u64,
}
impl offload::Identity for CryptoId {
    type Owner = WorkerId;
    fn owner(self) -> WorkerId {
        self.worker
    }
    fn generation(self) -> u64 {
        self.generation
    }
    fn sequence(self) -> u64 {
        self.sequence
    }
}
impl Ord for CryptoId {
    fn cmp(&self, other: &Self) -> std::cmp::Ordering {
        (self.worker.0, self.generation, self.sequence).cmp(&(
            other.worker.0,
            other.generation,
            other.sequence,
        ))
    }
}
impl PartialOrd for CryptoId {
    fn partial_cmp(&self, other: &Self) -> Option<std::cmp::Ordering> {
        Some(self.cmp(other))
    }
}
/// Output capacity was admitted on I/O. The engine cannot reach Admission,
/// flights, metadata catalogs, storage, credentials, or any Rc service graph.
pub enum CryptoInput {
    /// Check a stored CRC without allocating plaintext or running AEAD.
    Checksum { ciphertext: CiphertextPage },
    Decrypt {
        ciphertext: CiphertextPage,
        plaintext: flow_control::Charge<AdmissionPolicy>,
    },
    Encrypt {
        page: PageId,
        plaintext: PlaintextBuffer,
        ciphertext: flow_control::Charge<AdmissionPolicy>,
    },
}
pub enum CryptoOutput {
    Checksummed(CiphertextPage),
    /// Retain original ciphertext through completion too; I/O decides whether to
    /// retain it for peer copies/persistence after consuming the completion.
    Decrypted(VerifiedPage, CiphertextPage),
    Encrypted(VerifiedPage, CiphertextPage),
}
/// Non-cloneable, pair-bound reservation of both submission and completion space.
/// Only the I/O endpoint can mint it. Failed submission returns the entire job.
/// ```compile_fail
/// use racer_dataplane::security::CryptoPermit;
/// fn duplicate(permit: CryptoPermit) { let _second = permit.clone(); }
/// ```
pub struct CryptoPermit {
    pub(crate) send_sample: Option<crate::telemetry::Work>,
    measurement: Measurement,
    pub(crate) aead_failure: Option<crate::telemetry::AeadFailure>,
    reservation: offload::Permit<CryptoId>,
}
#[derive(Default)]
struct Measurement {
    checksum_only: bool,
    decrypt: bool,
    bytes: u64,
    submitted: Option<std::time::Instant>,
    queue_ns: Option<u64>,
    execution_ns: Option<u64>,
    rejection: Option<IntegrityRejection>,
}
/// Exact failed integrity check, not a diagnosis of where bytes became invalid.
#[derive(Clone, Copy)]
pub(crate) enum IntegrityRejection {
    Crc,
    Aead,
}
fn elapsed_ns(start: std::time::Instant) -> u64 {
    uring_runtime::environment::now()
        .saturating_duration_since(start)
        .as_nanos()
        .min(u128::from(u64::MAX)) as u64
}
impl CryptoPermit {
    pub(crate) fn execution_start(&self) -> Option<std::time::Instant> {
        Some(uring_runtime::environment::now())
    }
    pub(crate) fn executed(&mut self, start: std::time::Instant) {
        self.measurement.execution_ns = Some(elapsed_ns(start));
    }
    pub(crate) fn rejected(&mut self, rejection: IntegrityRejection) {
        self.measurement.rejection = Some(rejection);
    }
}
impl CryptoCompletion {
    /// Aggregate once on I/O dequeue, even for abandoned waiters. Started means
    /// execution observed through completion; engine loss is not a completion.
    fn record(&self, metrics: &crate::telemetry::Metrics) {
        use crate::telemetry::Event::*;
        let m = &self.permit.measurement;
        if m.checksum_only {
            return;
        }
        let Some(execution) = m.execution_ns else {
            return;
        };
        let events = if m.decrypt {
            [
                CryptoDecryptStarted,
                CryptoDecryptSuccess,
                CryptoDecryptFailure,
                CryptoDecryptBytes,
                CryptoDecryptExecutionCount,
                CryptoDecryptExecutionNs,
                CryptoDecryptQueueCount,
                CryptoDecryptQueueNs,
            ]
        } else {
            [
                CryptoEncryptStarted,
                CryptoEncryptSuccess,
                CryptoEncryptFailure,
                CryptoEncryptBytes,
                CryptoEncryptExecutionCount,
                CryptoEncryptExecutionNs,
                CryptoEncryptQueueCount,
                CryptoEncryptQueueNs,
            ]
        };
        let success = matches!(self.outcome, CryptoOutcome::Completed(_));
        let amounts = [
            1,
            u64::from(success),
            u64::from(!success),
            if success { m.bytes } else { 0 },
            1,
            execution,
            u64::from(m.queue_ns.is_some()),
            m.queue_ns.unwrap_or(0),
        ];
        for (event, amount) in events.into_iter().zip(amounts) {
            metrics.record(event, amount);
        }
        if let Some(rejection) = m.rejection {
            metrics.record(
                match rejection {
                    IntegrityRejection::Crc => CryptoDecryptCrcRejected,
                    IntegrityRejection::Aead => CryptoDecryptAeadRejected,
                },
                1,
            );
        }
    }
}
pub struct CryptoJob {
    pub(crate) input: CryptoInput,
    pub(crate) key: KeyLease,
    pub(crate) scope: RequestScope,
    // Engine loss drops payload owners before releasing diagnostic admission.
    pub(crate) permit: CryptoPermit,
}
impl CryptoPermit {
    pub fn job(mut self, input: CryptoInput, key: KeyLease, scope: RequestScope) -> CryptoJob {
        use uring_runtime::reactor::IoBuffer;
        self.measurement.checksum_only = matches!(input, CryptoInput::Checksum { .. });
        self.measurement.decrypt = matches!(input, CryptoInput::Decrypt { .. });
        self.measurement.bytes = match &input {
            CryptoInput::Encrypt { plaintext, .. } => {
                plaintext.bytes().map_or(0, |b| b.len() as u64)
            }
            CryptoInput::Decrypt { ciphertext, .. } | CryptoInput::Checksum { ciphertext } => {
                u64::from(ciphertext.envelope().plaintext_length)
            }
        };
        CryptoJob {
            permit: self,
            input,
            key,
            scope,
        }
    }
}
impl CryptoJob {
    pub fn id(&self) -> CryptoId {
        self.permit.reservation.id()
    }
}
impl offload::Reserved<CryptoId> for CryptoJob {
    fn permit(&self) -> &offload::Permit<CryptoId> {
        &self.permit.reservation
    }
}
/// Failed/canceled work returns its input allocations and reservations intact.
/// Successful work transfers those reservations to the output page leases.
pub enum CryptoOutcome {
    Completed(CryptoOutput),
    Failed { input: CryptoInput, error: Error },
}
/// Only the engine may create a completion after it stops accessing input.
/// The key and permit survive success AND failure until I/O consumes the result.
pub struct CryptoCompletion {
    pub(crate) outcome: CryptoOutcome,
    pub(crate) permit: CryptoPermit,
    pub(crate) _key: KeyLease,
}
impl CryptoCompletion {
    pub fn id(&self) -> CryptoId {
        self.permit.reservation.id()
    }
}
impl offload::Reserved<CryptoId> for CryptoCompletion {
    fn permit(&self) -> &offload::Permit<CryptoId> {
        &self.permit.reservation
    }
}
/// Endpoints move to their respective threads before constructing local services.
/// They are not Clone: exactly one producer/consumer exists in each direction.
pub struct IoCryptoPort {
    queue: offload::ClientPort<CryptoId, CryptoJob, CryptoCompletion>,
}
pub struct CryptoPort {
    queue: offload::WorkerPort<CryptoId, CryptoJob, CryptoCompletion>,
}
/// Crypto-thread resources, independent of the I/O worker service graph.
pub struct CryptoRuntime {
    /// Unique offload worker endpoint, transferred before local construction.
    pub port: CryptoPort,
}
/// Allocate fixed-capacity handoffs without starting either service.
pub fn pair(
    worker: WorkerId,
    generation: u64,
    capacity: NonZeroUsize,
) -> (IoCryptoPort, CryptoPort) {
    try_pair(worker, generation, capacity).expect("validated crypto queue capacity")
}
/// Fallible allocation for operational startup and untrusted configuration.
pub fn try_pair(
    worker: WorkerId,
    generation: u64,
    capacity: NonZeroUsize,
) -> Result<(IoCryptoPort, CryptoPort)> {
    let (client, worker) = offload::try_pair(
        CryptoId {
            worker,
            generation,
            sequence: 0,
        },
        capacity,
    )?;
    Ok((IoCryptoPort { queue: client }, CryptoPort { queue: worker }))
}
impl IoCryptoPort {
    /// Atomically reserve both directions. Saturation parks with a wakeup rather
    /// than holding a job-only slot. Reject wrong-worker/stale IDs.
    pub fn poll_reserve(&self, cx: &mut Context<'_>, id: CryptoId) -> Poll<Result<CryptoPermit>> {
        self.queue.poll_reserve(cx, id).map(|result| {
            result
                .map(|reservation| CryptoPermit {
                    send_sample: None,
                    aead_failure: None,
                    reservation,
                    measurement: Measurement::default(),
                })
                .map_err(Into::into)
        })
    }
    /// Rejected submission returns all ownership; accepted jobs outlive waiters.
    #[allow(clippy::result_large_err)] // Return the owned job without allocating on rejection.
    pub fn try_submit(
        &self,
        job: CryptoJob,
    ) -> std::result::Result<(), CryptoSendFailure<CryptoJob>> {
        // Capture at publication attempt, not reservation. Retry overwrites it.
        self.queue
            .try_submit(job, |job| {
                job.permit.measurement.submitted = Some(uring_runtime::environment::now());
            })
            .map_err(Into::into)
    }
    /// Drain even abandoned/stale completions before returning their credits.
    pub fn poll_completion(&self, cx: &mut Context<'_>) -> Poll<Result<Option<CryptoCompletion>>> {
        self.queue.poll_completion(cx).map_err(Into::into)
    }
    /// Refuse new reservations/submissions, but keep completions available.
    pub fn close_submissions(&self) -> Result<()> {
        self.queue.close_submissions();
        Ok(())
    }
}
impl CryptoPort {
    /// Worker-level wake registration independent of per-operation polling.
    pub fn register_driver(&self, waker: &Waker) {
        self.queue.register_driver(waker);
    }
    /// None means closed and all accepted jobs consumed, not temporarily empty.
    pub fn poll_job(&mut self, cx: &mut Context<'_>) -> Poll<Result<Option<CryptoJob>>> {
        let result = self.queue.poll_job(cx).map_err(Into::into);
        match result {
            Poll::Ready(Ok(Some(mut job))) => {
                job.permit.measurement.queue_ns = job.permit.measurement.submitted.map(elapsed_ns);
                Poll::Ready(Ok(Some(job)))
            }
            other => other,
        }
    }
    /// Reserved completion slots survive drain; failure returns engine ownership.
    #[allow(clippy::result_large_err)] // Preserve allocation-free ownership return on backpressure.
    pub fn complete(
        &mut self,
        completion: CryptoCompletion,
    ) -> std::result::Result<(), CryptoSendFailure<CryptoCompletion>> {
        self.queue.complete(completion).map_err(Into::into)
    }
}
/// Local submission facade driven by the worker, not by the waiting future.
/// Future drop abandons delivery only; completion reap fences retained resources.
/// I/O checks generation/sequence before delivery and never publishes stale work.
pub struct CryptoClient {
    observer: RefCell<crate::telemetry::Observer>,

    port: IoCryptoPort,

    metrics: RefCell<Option<crate::telemetry::Metrics>>,

    executor: offload::Client<CryptoId, CryptoCompletion, RequestScope>,
}
impl CryptoClient {
    pub fn new(port: IoCryptoPort) -> Self {
        let executor = offload::Client::new(NonZeroUsize::new(port.queue.capacity()).unwrap());
        Self {
            observer: RefCell::new(crate::telemetry::Observer::default()),
            port,
            metrics: RefCell::new(None),
            executor,
        }
    }
    /// Install the I/O writer before admission; crypto never writes this shard.
    pub(crate) fn set_metrics(&self, metrics: crate::telemetry::Metrics) {
        *self.metrics.borrow_mut() = Some(metrics);
    }
    pub(crate) fn set_failure_observer(&self, observer: crate::telemetry::Observer) {
        *self.observer.borrow_mut() = observer;
    }
    /// Reserve both queue slots and register before enqueue. Sequence overflow
    /// requires drain/restart, never wrap. The owned job retains the original scope.
    pub fn execute<'a>(
        &'a self,
        input: CryptoInput,
        key: KeyLease,
        scope: &'a RequestScope,
    ) -> Operation<'a, CryptoOutput> {
        self.execute_sample(input, key, scope, None)
    }
    pub(crate) fn execute_sample<'a>(
        &'a self,
        input: CryptoInput,
        key: KeyLease,
        scope: &'a RequestScope,
        sample: Option<crate::telemetry::Work>,
    ) -> Operation<'a, CryptoOutput> {
        Box::pin(async move {
            let completion = self
                .executor
                .execute(
                    &self.port.queue,
                    scope,
                    |sequence| CryptoId {
                        worker: self.port.queue.identity().worker,
                        generation: self.port.queue.identity().generation,
                        sequence,
                    },
                    |reservation| {
                        if let Some(sample) = &sample {
                            sample.identify(reservation.id());
                        }
                        CryptoPermit {
                            reservation,
                            send_sample: sample,
                            aead_failure: None,
                            measurement: Measurement::default(),
                        }
                        .job(input, key, scope.clone())
                    },
                    |job| {
                        job.permit.measurement.submitted = Some(uring_runtime::environment::now());
                    },
                )
                .await?;
            match completion.outcome {
                CryptoOutcome::Completed(output) => Ok(output),
                CryptoOutcome::Failed { error, .. } => Err(error),
            }
        })
    }
    /// I/O reaps even when no user futures remain, before admitting more work.
    pub fn poll_budgeted(&self, work_budget: usize) -> Result<()> {
        self.executor
            .poll_budgeted(&self.port.queue, work_budget, |completion| {
                self.observe(completion)
            })
    }
    /// Record application telemetry on the owning I/O shard before delivery.
    fn observe(&self, completion: &CryptoCompletion) {
        if let Some(metrics) = self.metrics.borrow().as_ref() {
            completion.record(metrics);
        }
        let id = completion.id();
        if let Some(sample) = &completion.permit.send_sample {
            sample.finish(match &completion.outcome {
                CryptoOutcome::Failed { error, .. } => Some(*error),
                _ => None,
            });
            // Permit retains the owner through reap and completion consumption.
        }
        if let Some(failure) = completion.permit.aead_failure {
            self.observer.borrow().record_aead(id, failure);
        }
    }
    pub fn outstanding(&self) -> usize {
        self.port.queue.outstanding()
    }
    pub fn register_driver(&self, waker: &Waker) {
        self.port.queue.register_driver(waker);
    }
    pub fn close_submissions(&self) -> Result<()> {
        self.port.close_submissions()
    }
    /// Deadline cancels delivery, not the ownership fence. Keep polling until all
    /// accepted jobs complete; a timeout cannot authorize dropping live buffers.
    pub fn drain<'a>(&'a self, scope: &'a RequestScope) -> Operation<'a, ()> {
        Box::pin(self.executor.drain(&self.port.queue, scope, |completion| {
            self.observe(completion)
        }))
    }
}
#[cfg(test)]
mod tests {
    use super::*;
    use crate::model::*;
    use racer_control_wire::CacheId;
    use racer_control_wire::KeyId;
    use uring_runtime::group::Service;
    #[test]
    fn opaque_context_round_trips_non_utf8_without_normalization() {
        let bytes = b"opaque,  credential\\\"\xff";
        let authorization = Authorization::from_header(bytes).unwrap();
        let metadata = OpaqueMetadata::from_header(bytes).unwrap();
        assert_eq!(authorization.expose_for_origin(), bytes);
        assert_eq!(metadata.as_header(), bytes);
        let context = OriginContext {
            object: ObjectId {
                cache: CacheId("cache".into()),
                key: CacheKey([0; 32]),
            },
            authorization: Some(authorization),
            metadata: Some(metadata),
        };
        assert_eq!(format!("{context:?}"), "OriginContext([redacted])");
        assert_eq!(
            format!("{:#?}", context.authorization.unwrap()),
            "Authorization([redacted])"
        );
        assert_eq!(
            format!("{:#?}", context.metadata.unwrap()),
            "OpaqueMetadata([redacted])"
        );
    }
    #[test]
    fn context_rejects_present_empty_controls_padding_and_oversize() {
        for bytes in [
            b"".as_slice(),
            b" leading",
            b"trailing ",
            b"a\tb",
            b"a\rb",
            b"a\nb",
            b"a\0b",
            b"a\x7fb",
        ] {
            assert!(matches!(
                Authorization::from_header(bytes),
                Err(Error::InvalidRequest)
            ));
            assert!(matches!(
                OpaqueMetadata::from_header(bytes),
                Err(Error::InvalidRequest)
            ));
        }
        for length in [MAX_FIELD_BYTES, MAX_FIELD_BYTES + 1] {
            let bytes = vec![b'x'; length];
            assert_eq!(
                Authorization::from_header(&bytes).is_ok(),
                length == MAX_FIELD_BYTES
            );
            assert_eq!(
                OpaqueMetadata::from_header(&bytes).is_ok(),
                length == MAX_FIELD_BYTES
            );
        }
    }
    #[test]
    fn sensitive_storage_uses_zeroizing_owners() {
        fn zeroizing(_: &Zeroizing<Vec<u8>>) {}
        zeroizing(&Authorization::from_header(b"secret").unwrap().bytes);
        zeroizing(&OpaqueMetadata::from_header(b"private").unwrap().bytes);
    }
    mod credentials {
        use super::*;
        use crate::model::CacheKey;
        use racer_control_wire::CacheId;

        #[test]
        fn local_context_is_independently_charged_and_keeps_exact_sensitive_fields() {
            let keys = Rc::new(crate::test_support::security::keys());
            let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
                crate::test_support::cluster::config(false).limits,
            )));
            let crypto = CredentialCrypto::new(keys, admission.clone());
            let scope = RequestScope::new(
                RequestId([4; 16]),
                std::time::Instant::now() + std::time::Duration::from_secs(5),
            )
            .unwrap();
            let context = OriginContext {
                object: ObjectId {
                    cache: CacheId("00000000-0000-4000-8000-000000000001".into()),
                    key: CacheKey([3; 32]),
                },
                metadata: Some(OpaqueMetadata::from_header(b"meta\xff").unwrap()),
                authorization: Some(Authorization::from_header(b"opaque\x80").unwrap()),
            };
            let first = crypto.local_context(&context, &scope).unwrap();
            let used = admission.used(ResourceClass::RequestContext);
            let second = crypto.local_context(&context, &scope).unwrap();
            assert_eq!(admission.used(ResourceClass::RequestContext), 2 * used);
            drop(context);
            assert_eq!(
                first.authorization.as_ref().unwrap().expose_for_origin(),
                b"opaque\x80"
            );
            assert_eq!(second.metadata.as_ref().unwrap().as_header(), b"meta\xff");
            scope.cancel().unwrap();
            assert!(matches!(
                crypto.local_context(&first, &scope),
                Err(Error::Cancelled)
            ));
            drop((first, second));
            assert_eq!(admission.used(ResourceClass::RequestContext), 0);
        }
        fn scope() -> RequestScope {
            RequestScope::new(
                RequestId([1; 16]),
                std::time::Instant::now() + std::time::Duration::from_secs(10),
            )
            .unwrap()
        }
        fn origin() -> OriginContext {
            OriginContext {
                object: ObjectId {
                    cache: CacheId(crate::test_support::security::CACHE.into()),
                    key: CacheKey([3; 32]),
                },
                metadata: Some(OpaqueMetadata::from_header(b"opaque,  \xff").unwrap()),
                authorization: Some(Authorization::from_header(b"Bearer exact  \xfe").unwrap()),
            }
        }
        #[test]
        fn exact_roundtrip_nonce_freshness_admission_and_cancellation() {
            let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
                crate::test_support::cluster::config(false).limits,
            )));
            let crypto = CredentialCrypto::new(
                Rc::new(crate::test_support::security::keys()),
                admission.clone(),
            );
            let original = origin();
            let scope = scope();
            let attempt = AttemptId([4; 16]);
            let first = crypto.seal(&original, attempt, &scope).unwrap();
            let second = crypto.seal(&original, attempt, &scope).unwrap();
            assert_ne!(
                first.authorization.as_ref().unwrap().nonce,
                second.authorization.as_ref().unwrap().nonce
            );
            drop(second);
            let clear = crypto.open_charged(first, scope.request, attempt).unwrap();
            assert_eq!(
                clear.authorization.as_ref().unwrap().expose_for_origin(),
                b"Bearer exact  \xfe"
            );
            assert_eq!(
                clear.metadata.as_ref().unwrap().as_header(),
                b"opaque,  \xff"
            );
            assert!(admission.used(ResourceClass::RequestContext) > 0);
            drop(clear);
            assert_eq!(admission.used(ResourceClass::RequestContext), 0);
            scope.cancel().unwrap();
            assert!(matches!(
                crypto.seal(&original, attempt, &scope),
                Err(Error::Cancelled)
            ));
            assert_eq!(admission.used(ResourceClass::RequestContext), 0);
            let mut limits = crate::test_support::cluster::config(false).limits;
            limits.request_context_bytes = std::num::NonZeroUsize::new(1).unwrap();
            let crypto = CredentialCrypto::new(
                Rc::new(crate::test_support::security::keys()),
                Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(limits))),
            );
            assert!(matches!(
                crypto.seal(&original, attempt, &self::scope()),
                Err(Error::Overloaded)
            ));
        }
        #[test]
        fn absent_authorization_is_charged_and_expired_context_is_rejected() {
            let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
                crate::test_support::cluster::config(false).limits,
            )));
            let crypto = CredentialCrypto::new(
                Rc::new(crate::test_support::security::keys()),
                admission.clone(),
            );
            let mut original = origin();
            original.authorization = None;
            let scope = scope();
            let attempt = AttemptId([4; 16]);
            let sealed = crypto.seal(&original, attempt, &scope).unwrap();
            assert!(sealed.authorization.is_none());
            assert!(admission.used(ResourceClass::RequestContext) > 0);
            let clear = crypto.open_charged(sealed, scope.request, attempt).unwrap();
            assert!(clear.authorization.is_none());
            assert_eq!(
                clear.metadata.as_ref().unwrap().as_header(),
                b"opaque,  \xff"
            );
            drop(clear);
            assert_eq!(admission.used(ResourceClass::RequestContext), 0);
            let sealed = crypto.seal(&original, attempt, &scope).unwrap();
            scope.cancel().unwrap();
            assert!(matches!(
                crypto.open_charged(sealed, scope.request, attempt),
                Err(Error::Cancelled)
            ));
            assert_eq!(admission.used(ResourceClass::RequestContext), 0);
        }
        #[test]
        fn rejects_substitution_and_distinguishes_absent_from_empty() {
            let crypto = CredentialCrypto::new(
                Rc::new(crate::test_support::security::keys()),
                Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
                    crate::test_support::cluster::config(false).limits,
                ))),
            );
            let original = origin();
            let scope = scope();
            let attempt = AttemptId([4; 16]);
            for index in 0..8 {
                let mut sealed = crypto.seal(&original, attempt, &scope).unwrap();
                match index {
                    0 => sealed.object.key.0[0] ^= 1,
                    1 => sealed.metadata = None,
                    2 => sealed.authorization.as_mut().unwrap().ciphertext[0] ^= 1,
                    3 => sealed.authorization.as_mut().unwrap().nonce.0[0] ^= 1,
                    4 => sealed.request.0[0] ^= 1,
                    5 => sealed.attempt.0[0] ^= 1,
                    6 => sealed
                        .authorization
                        .as_mut()
                        .unwrap()
                        .ciphertext
                        .truncate(TAG_LEN - 1),
                    _ => sealed
                        .authorization
                        .as_mut()
                        .unwrap()
                        .ciphertext
                        .truncate(TAG_LEN),
                }
                assert!(crypto.open_charged(sealed, scope.request, attempt).is_err());
            }
            assert_ne!(
                *credential_aad(
                    KeyId([1; 16]),
                    &original.object,
                    scope.request,
                    attempt,
                    None
                )
                .unwrap(),
                *credential_aad(
                    KeyId([1; 16]),
                    &original.object,
                    scope.request,
                    attempt,
                    Some(b"")
                )
                .unwrap()
            );
        }
        #[test]
        fn seal_borrows_inputs_and_returns_an_independent_owner() {
            // Independent input lifetimes; the result cannot borrow any of them.
            let _: for<'crypto, 'origin, 'scope> fn(
                &'crypto CredentialCrypto,
                &'origin OriginContext,
                AttemptId,
                &'scope RequestScope,
            ) -> Result<PeerOriginContext> = CredentialCrypto::seal;
            fn owned<T: Send + 'static>() {}
            owned::<PeerOriginContext>();
        }
    }
    mod crypto {
        //! Owned page-AEAD handoff between one I/O shard and its crypto engine endpoint.
        //! Multiple endpoints can share a NUMA-local crypto execution thread.
        //!
        //! Queue ownership and completion admission are independent of waiting futures.
        //! Admission stays on I/O. Reserve a job slot AND its eventual completion slot
        //! before enqueueing. The permit follows the job through completion consumption,
        //! so cancellation, an abandoned future, or shutdown cannot reclaim its capacity,
        //! buffers, or key early. Completion publication never waits for new admission.
        //!
        //! All polling must register the waker and recheck state before returning Pending.
        //! Enqueue wakes crypto; completion wakes I/O; consumption wakes capacity waiters;
        //! close wakes both sides. Neither side spins or blocks awaiting the other. This
        //! is required even when both OS threads share one allowed CPU.
        //!
        //! Queue timing measures accepted submission to crypto dequeue, excluding permit
        //! waits and execution. I/O records each measured execution once on completion
        //! reap, including failed, canceled, or abandoned work reaching that path. Queued
        //! or in-flight work and engine loss without completion are not yet counted.
        //! The racer_crypto_{encrypt,decrypt}_queue_nanoseconds_{sum,count} counters yield
        //! mean wait in milliseconds as rate(sum) / rate(count) / 1e6. For a fleet mean,
        //! sum each rate across instances before dividing. Zero count rate has no defined
        //! mean; these counters provide neither percentiles nor current queue age or total
        //! request latency.

        use crate::security::*;
        use racer_control_wire::CacheId;

        #[cfg(test)]
        mod measurement {
            //! Measurement correctness scenarios using the real page engine.
            use super::tests::input;
            use super::tests::keyring;
            use super::*;
            use crate::admission::ResourceClass;
            use crate::memory::BufferPool;
            use crate::model::Nonce;
            use crate::model::PageEnvelope;
            use crate::model::*;
            use crate::security::PageCryptoEngine;
            use crate::security::page_aad;
            use crate::telemetry::Event;
            use crate::telemetry::Event::*;
            use crate::telemetry::Metrics;
            use racer_crypto::TAG_LEN;
            use std::rc::Rc;
            use std::time::Duration;
            use std::time::Instant;
            use uring_runtime::reactor::IoBuffer;

            fn page() -> PageId {
                PageId {
                    version: ObjectVersion {
                        object: ObjectId {
                            cache: CacheId("00000000-0000-4000-8000-000000000003".into()),
                            key: CacheKey([0; 32]),
                        },
                        etag: StrongEtag::test_value("measurement"),
                    },
                    number: PageNumber(0),
                }
            }
            fn envelope(size: usize) -> PageEnvelope {
                PageEnvelope {
                    page: page(),
                    key_id: crate::model::key_id_from_generation(1, 1).unwrap(),
                    nonce: Nonce([2; 24]),
                    plaintext_length: size as u32,
                    ciphertext_length: size as u32 + 16,
                }
            }

            fn validate(result: &CryptoCompletion, size: usize) {
                if let CryptoOutcome::Completed(output) = &result.outcome {
                    let (CryptoOutput::Encrypted(plain, ciphertext)
                    | CryptoOutput::Decrypted(plain, ciphertext)) = output
                    else {
                        panic!("AEAD measurement received checksum-only output");
                    };
                    assert_eq!(plain.bytes().len(), size);
                    assert_eq!(ciphertext.bytes().len(), size + 16);
                    assert_eq!(plain.bytes()[0], 7);
                    assert_eq!(plain.bytes()[size - 1], 7);
                }
            }
            fn lifecycle(size: usize, paired: bool, operation: &str) {
                let inputs = MeasurementInputs::new(size, operation);
                let admission = &inputs.admission;
                let cache = &inputs.page.version.object.cache;
                let keys = tests::keyring();
                let iterations = 16;
                let concurrency = if paired { 8 } else { 1 };
                let (io, mut engine) =
                    pair(WorkerId(0), 0, NonZeroUsize::new(concurrency).unwrap());
                let thread = if paired {
                    Some(std::thread::spawn(move || {
                        loop {
                            let job = futures::executor::block_on(futures::future::poll_fn(|cx| {
                                engine.poll_job(cx)
                            }))
                            .unwrap();
                            let Some(job) = job else { break };
                            assert!(engine.complete(PageCryptoEngine::process(job)).is_ok());
                        }
                    }))
                } else {
                    None
                };
                let scope = RequestScope::new(
                    RequestId([0; 16]),
                    Instant::now() + Duration::from_secs(240),
                )
                .unwrap();
                let mut failures = 0;
                let mut completed = 0;
                for i in 0..iterations {
                    let input = inputs.input(operation);
                    let permit = futures::executor::block_on(futures::future::poll_fn(|cx| {
                        io.poll_reserve(
                            cx,
                            CryptoId {
                                worker: WorkerId(0),
                                generation: 0,
                                sequence: i as u64,
                            },
                        )
                    }))
                    .unwrap();
                    let job = permit.job(
                        input,
                        keys.active(cache, racer_crypto::identity::KeyPurpose::Page)
                            .unwrap(),
                        scope.clone(),
                    );
                    if paired {
                        assert!(io.try_submit(job).is_ok());
                    } else {
                        let result = PageCryptoEngine::process(job);
                        validate(&result, size);
                        failures +=
                            usize::from(matches!(result.outcome, CryptoOutcome::Failed { .. }));
                        drop(result);
                        completed += 1;
                    }
                    if paired && (i + 1 - completed == concurrency || i + 1 == iterations) {
                        while completed <= i {
                            let result =
                                futures::executor::block_on(futures::future::poll_fn(|cx| {
                                    io.poll_completion(cx)
                                }))
                                .unwrap()
                                .unwrap();
                            validate(&result, size);
                            failures +=
                                usize::from(matches!(result.outcome, CryptoOutcome::Failed { .. }));
                            drop(result);
                            completed += 1;
                        }
                    }
                }
                io.close_submissions().unwrap();
                if let Some(thread) = thread {
                    thread.join().unwrap();
                }
                admission.reclaim_buffers();
                assert_eq!(
                    failures,
                    if operation == "bad_tag" {
                        iterations
                    } else {
                        0
                    }
                );
                assert_eq!(admission.used(ResourceClass::Plaintext), 0);
                assert_eq!(admission.used(ResourceClass::Ciphertext), 0);
            }

            #[test]
            fn lifecycle_setup_preserves_success_failure_and_quota_cleanup() {
                for paired in [false, true] {
                    for operation in ["encrypt", "decrypt", "bad_tag"] {
                        lifecycle(4095, paired, operation);
                    }
                }
            }

            /// Full client submission, waiter registration, real paired execution, I/O reap,
            /// result delivery, and cleanup for both encrypt and decrypt attribution.
            fn accounting_sample(size: usize, decrypt: bool, iterations: usize) {
                let operation = if decrypt { "decrypt" } else { "encrypt" };
                let inputs = MeasurementInputs::new(size, operation);
                let admission = &inputs.admission;
                let keys = tests::keyring();
                let cache = &inputs.page.version.object.cache;
                let metrics = Metrics::default();
                let (io, mut engine) = pair(WorkerId(0), 0, NonZeroUsize::new(8).unwrap());
                let client = CryptoClient::new(io);
                client.set_metrics(metrics.clone());
                let environment = uring_runtime::environment::Environment::current();
                let thread = std::thread::spawn(move || {
                    let _env = environment.enter();
                    while let Some(job) =
                        futures::executor::block_on(futures::future::poll_fn(|cx| {
                            engine.poll_job(cx)
                        }))
                        .unwrap()
                    {
                        assert!(engine.complete(PageCryptoEngine::process(job)).is_ok());
                    }
                });
                let scope = RequestScope::new(
                    RequestId([0; 16]),
                    uring_runtime::environment::now() + Duration::from_secs(240),
                )
                .unwrap();
                for batch in (0..iterations).step_by(8) {
                    let operations = (batch..(batch + 8).min(iterations)).map(|_| async {
                        let input = inputs.input(operation);
                        let output = client
                            .execute(
                                input,
                                keys.active(cache, racer_crypto::identity::KeyPurpose::Page)
                                    .unwrap(),
                                &scope,
                            )
                            .await
                            .unwrap();
                        let (CryptoOutput::Encrypted(plain, cipher)
                        | CryptoOutput::Decrypted(plain, cipher)) = &output
                        else {
                            panic!("AEAD measurement received checksum-only output");
                        };
                        assert_eq!(plain.bytes().len(), size);
                        assert_eq!((plain.bytes()[0], plain.bytes()[size - 1]), (7, 7));
                        assert_eq!(cipher.bytes().len(), size + 16);
                        drop(output);
                    });
                    let mut all = Box::pin(futures::future::join_all(operations));
                    futures::executor::block_on(futures::future::poll_fn(|cx| {
                        client.register_driver(cx.waker());
                        client.poll_budgeted(8).unwrap();
                        std::future::Future::poll(all.as_mut(), cx)
                    }));
                }
                client.close_submissions().unwrap();
                thread.join().unwrap();
                admission.reclaim_buffers();
                assert_eq!(client.outstanding(), 0);
                assert!(client.executor.is_empty());
                assert_eq!(admission.used(ResourceClass::Plaintext), 0);
                assert_eq!(admission.used(ResourceClass::Ciphertext), 0);
                let events = measurement_events(decrypt);
                for (event, expected) in events.into_iter().zip([
                    iterations as u64,
                    iterations as u64,
                    0,
                    (iterations * size) as u64,
                    iterations as u64,
                    metrics.count(events[5]),
                    iterations as u64,
                    metrics.count(events[7]),
                ]) {
                    assert_eq!(metrics.count(event), expected);
                }
                if uring_runtime::environment::simulation_seed().is_none() {
                    assert!(metrics.count(events[5]) > 0);
                    assert!(metrics.count(events[7]) > 0);
                }
            }

            #[test]
            fn attribution_preserves_client_cleanup_and_dst() {
                use uring_runtime::environment;
                use uring_runtime::environment::SimulationClock;
                let clock = SimulationClock::new(73);
                let env = clock.environment(0);
                let _env = env.enter();
                let _strict = environment::require_simulated();
                for decrypt in [false, true] {
                    accounting_sample(63, decrypt, 16);
                }
            }

            #[test]
            fn measurements_account_once_at_reap_even_for_cancel_and_abandon() {
                use std::time::Duration;
                use uring_runtime::environment;
                use uring_runtime::environment::SimulationClock;
                let clock = SimulationClock::new(42);
                let env = clock.environment(0);
                let _env = env.enter();
                let _strict = environment::require_simulated();
                let admission = std::rc::Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
                    crate::test_support::cluster::config(false).limits,
                )));
                let keys = keyring();
                for decrypt in [false, true] {
                    for mode in [
                        "success",
                        "failure",
                        "cancel",
                        "abandon",
                        "aead",
                        "malformed",
                        "short_body",
                        "abandon_crc",
                        "abandon_aead",
                    ] {
                        if !decrypt
                            && matches!(
                                mode,
                                "aead"
                                    | "malformed"
                                    | "short_body"
                                    | "abandon_crc"
                                    | "abandon_aead"
                            )
                        {
                            continue;
                        }
                        let (io, mut engine) = pair(WorkerId(0), 0, NonZeroUsize::new(1).unwrap());
                        let client = CryptoClient::new(io);
                        let metrics = Metrics::default();
                        client.set_metrics(metrics.clone());
                        let failures = crate::telemetry::Failures::default();
                        client.set_failure_observer(failures.observer(WorkerId(0)));
                        let scope = RequestScope::new(
                            crate::model::RequestId([0; 16]),
                            environment::now() + Duration::from_secs(10),
                        )
                        .unwrap();
                        let cache = racer_control_wire::CacheId(
                            "00000000-0000-4000-8000-000000000003".into(),
                        );
                        let lease = || {
                            keys.active(&cache, racer_crypto::identity::KeyPurpose::Page)
                                .unwrap()
                        };
                        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
                        let mut data = measurement_input(
                            &admission,
                            &cache,
                            lease(),
                            &scope,
                            decrypt,
                            matches!(mode, "failure" | "aead" | "abandon_crc" | "abandon_aead"),
                        );
                        if let CryptoInput::Decrypt { ciphertext, .. } = &mut data {
                            if matches!(mode, "aead" | "abandon_aead") {
                                // Peer bytes without a persisted CRC must still fail AEAD.
                                Arc::get_mut(&mut ciphertext.inner).unwrap().checksum =
                                    std::sync::OnceLock::new();
                            }
                            if mode == "malformed" {
                                Arc::get_mut(&mut ciphertext.inner)
                                    .unwrap()
                                    .envelope
                                    .ciphertext_length += 1;
                            }
                            if mode == "short_body" {
                                // A valid descriptor with a mismatched body is malformed,
                                // not an authentication rejection from the primitive.
                                let inner = Arc::get_mut(&mut ciphertext.inner).unwrap();
                                inner.checksum = std::sync::OnceLock::new();
                                inner.bytes.pop();
                            }
                        }
                        // Admission delay must not leak into residence.
                        clock.advance(Duration::from_millis(100));
                        let mut future = client.execute(data, lease(), &scope);
                        assert!(future.as_mut().poll(&mut cx).is_pending());
                        if mode == "cancel" {
                            scope.cancel().unwrap();
                        }
                        clock.advance(Duration::from_millis(3));
                        let Poll::Ready(Ok(Some(job))) = engine.poll_job(&mut cx) else {
                            panic!("job")
                        };
                        let mut completion = crate::security::PageCryptoEngine::process(job);
                        // The engine uses virtual time (zero cost in DST). Explicitly
                        // advance a measured interval to exercise the exact sum separately.
                        assert_eq!(completion.permit.measurement.execution_ns, Some(0));
                        let start = environment::now();
                        clock.advance(Duration::from_millis(2));
                        completion.permit.executed(start);
                        let (_, mut wrong) = pair(WorkerId(1), 0, NonZeroUsize::new(1).unwrap());
                        completion = wrong.complete(completion).err().unwrap().command;
                        assert!(engine.complete(completion).is_ok());
                        let events = measurement_events(decrypt);
                        assert_eq!(metrics.count(events[0]), 0);
                        assert_eq!(metrics.count(CryptoDecryptCrcRejected), 0);
                        assert_eq!(metrics.count(CryptoDecryptAeadRejected), 0);
                        if matches!(mode, "abandon" | "abandon_crc" | "abandon_aead") {
                            drop(future);
                        } else {
                            client.poll_budgeted(1).unwrap();
                            match future.as_mut().poll(&mut cx) {
                                Poll::Ready(Ok(_)) => assert_eq!(mode, "success"),
                                Poll::Ready(Err(error)) => assert_eq!(
                                    error,
                                    if mode == "cancel" {
                                        Error::Cancelled
                                    } else if !decrypt {
                                        Error::MissingKey
                                    } else {
                                        Error::CorruptRecord
                                    }
                                ),
                                Poll::Pending => panic!("completion not returned"),
                            }
                        }
                        client.poll_budgeted(1).unwrap();
                        client.poll_budgeted(1).unwrap();
                        let success = u64::from(mode == "success" || mode == "abandon");
                        for (event, expected) in events.into_iter().zip([
                            1,
                            success,
                            1 - success,
                            success,
                            1,
                            2_000_000,
                            1,
                            3_000_000,
                        ]) {
                            assert_eq!(metrics.count(event), expected, "{mode} {event:?}");
                        }
                        assert_eq!(client.outstanding(), 0);
                        assert_eq!(
                            metrics.count(CryptoDecryptCrcRejected),
                            u64::from(decrypt && matches!(mode, "failure" | "abandon_crc")),
                            "{mode}"
                        );
                        assert_eq!(
                            metrics.count(CryptoDecryptAeadRejected),
                            u64::from(matches!(mode, "aead" | "abandon_aead")),
                            "{mode}"
                        );
                        let mut records = String::new();
                        failures.write_aead(&mut records).unwrap();
                        let rejected = matches!(mode, "aead" | "abandon_aead");
                        assert_eq!(records.lines().count(), 1 + usize::from(rejected), "{mode}");
                        assert!(records.starts_with(if rejected {
                            "total=1 retained=1"
                        } else {
                            "total=0 retained=0"
                        }));
                        if rejected {
                            assert!(!records.contains("crc=none"));
                        }
                    }
                }
            }

            #[test]
            fn send_crc_computes_fresh_body_and_holds_owner_through_abandoned_reap() {
                use crate::security::PageCryptoEngine;
                use crate::telemetry::Pair;
                use crate::telemetry::Samples;
                use uring_runtime::environment;
                let admission = std::rc::Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
                    crate::test_support::cluster::config(false).limits,
                )));
                let keys = keyring();
                let cache =
                    racer_control_wire::CacheId("00000000-0000-4000-8000-000000000003".into());
                let lease = || {
                    keys.active(&cache, racer_crypto::identity::KeyPurpose::Page)
                        .unwrap()
                };
                let scope = RequestScope::new(
                    crate::model::RequestId([7; 16]),
                    environment::now() + Duration::from_secs(10),
                )
                .unwrap();
                let mut cx = Context::from_waker(futures::task::noop_waker_ref());
                let (setup, _engine) = pair(WorkerId(0), 0, NonZeroUsize::new(1).unwrap());
                let Poll::Ready(Ok(permit)) = setup.poll_reserve(
                    &mut cx,
                    CryptoId {
                        worker: WorkerId(0),
                        generation: 0,
                        sequence: 1,
                    },
                ) else {
                    panic!("permit")
                };
                let completion = PageCryptoEngine::process(permit.job(
                    input(&admission),
                    lease(),
                    scope.clone(),
                ));
                let CryptoOutcome::Completed(CryptoOutput::Encrypted(plain, mut ciphertext)) =
                    completion.outcome
                else {
                    panic!("encrypted")
                };
                drop(plain);
                Arc::get_mut(&mut ciphertext.inner).unwrap().checksum = std::sync::OnceLock::new();
                let expected = racer_crypto::crc64(ciphertext.bytes());
                let retained = ciphertext.clone();
                let samples = Samples::default();
                let filter = Pair::parse(
                    "8816d91d-e896-49bf-ba8a-da97ede93818,11111111-1111-4111-8111-111111111111",
                )
                .unwrap();
                let (ticket, sample) = samples
                    .begin(&filter, &filter.sender, &filter.receiver)
                    .unwrap();
                ticket.finish(true);
                drop(ticket);
                let (io, mut engine) = pair(WorkerId(0), 1, NonZeroUsize::new(1).unwrap());
                let client = CryptoClient::new(io);
                let mut future = client.execute_sample(
                    CryptoInput::Checksum { ciphertext },
                    lease(),
                    &scope,
                    Some(sample),
                );
                assert!(future.as_mut().poll(&mut cx).is_pending());
                assert_eq!(retained.cached_checksum(), None);
                drop(future);
                let Poll::Ready(Ok(Some(job))) = engine.poll_job(&mut cx) else {
                    panic!("job")
                };
                let completion = PageCryptoEngine::process(job);
                assert_eq!(retained.cached_checksum(), Some(expected));
                assert!(engine.complete(completion).is_ok());
                let mut text = String::new();
                samples.write(&mut text).unwrap();
                assert!(text.contains("busy=1"));
                assert!(text.contains("status=pending"));
                client.poll_budgeted(1).unwrap();
                client.poll_budgeted(1).unwrap();
                text.clear();
                samples.write(&mut text).unwrap();
                assert!(text.contains("busy=0"));
                assert!(text.contains("send=completed status=computed"));
                assert!(text.contains(&format!("crc={expected:016x}")));
                assert_eq!(text.lines().count(), 2);
                assert_eq!(client.outstanding(), 0);
            }

            #[test]
            fn send_crc_cached_cancel_missing_key_and_capacity_are_diagnostic_only() {
                use crate::security::PageCrypto;
                use crate::security::PageCryptoEngine;
                use crate::telemetry::Pair;
                use crate::telemetry::Samples;
                let admission = std::rc::Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
                    crate::test_support::cluster::config(false).limits,
                )));
                let keys = std::rc::Rc::new(keyring());
                let filter = Pair::parse(
                    "8816d91d-e896-49bf-ba8a-da97ede93818,11111111-1111-4111-8111-111111111111",
                )
                .unwrap();
                for mode in ["cached", "cancel", "missing", "capacity"] {
                    let mut page = crate::memory::tests::bundle_for(
                        &admission,
                        crate::model::VersionMetadata {
                            content_type: None,
                            version: crate::model::ObjectVersion {
                                object: crate::model::ObjectId {
                                    cache: racer_control_wire::CacheId(
                                        "00000000-0000-4000-8000-000000000003".into(),
                                    ),
                                    key: crate::model::CacheKey([3; 32]),
                                },
                                etag: crate::model::StrongEtag::test_value("send-crc"),
                            },
                            length: 3,
                        },
                    )
                    .ciphertext;
                    let inner = Arc::get_mut(&mut page.inner).unwrap();
                    inner.envelope.page.version.object.cache =
                        racer_control_wire::CacheId("00000000-0000-4000-8000-000000000003".into());
                    if mode == "cached" {
                        inner.checksum.set(42).unwrap();
                    }
                    if mode == "missing" {
                        // The fixture keyring installs [1;16], also the bundle's default.
                        inner.envelope.key_id = racer_control_wire::KeyId([99; 16]);
                    }
                    let cache = page.envelope().page.version.object.cache.clone();
                    let lease = keys
                        .active(&cache, racer_crypto::identity::KeyPurpose::Page)
                        .unwrap();
                    let scope = RequestScope::new(
                        crate::model::RequestId([7; 16]),
                        uring_runtime::environment::now() + Duration::from_secs(10),
                    )
                    .unwrap();
                    let (io, mut engine) = pair(WorkerId(0), 1, NonZeroUsize::new(1).unwrap());
                    let client = std::rc::Rc::new(CryptoClient::new(io));
                    let samples = Samples::default();
                    let (ticket, sample) = samples
                        .begin(&filter, &filter.sender, &filter.receiver)
                        .unwrap();
                    ticket.finish(true);
                    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
                    if mode == "missing" {
                        let crypto = PageCrypto::new(keys.clone(), client.clone());
                        let mut future = Box::pin(crypto.sample_send(page, &scope, sample));
                        assert!(
                            future.as_mut().poll(&mut cx).is_ready(),
                            "missing-key work must not enqueue"
                        );
                    } else {
                        let held = if mode == "capacity" {
                            let Poll::Ready(Ok(permit)) = client.port.poll_reserve(
                                &mut cx,
                                CryptoId {
                                    worker: WorkerId(0),
                                    generation: 1,
                                    sequence: 0,
                                },
                            ) else {
                                panic!("permit")
                            };
                            Some(permit)
                        } else {
                            None
                        };
                        let mut future = client.execute_sample(
                            CryptoInput::Checksum { ciphertext: page },
                            lease,
                            &scope,
                            Some(sample),
                        );
                        assert!(future.as_mut().poll(&mut cx).is_pending());
                        if mode == "capacity" {
                            scope.cancel().unwrap();
                            assert!(matches!(
                                future.as_mut().poll(&mut cx),
                                Poll::Ready(Err(Error::Cancelled))
                            ));
                            drop(future);
                            drop(held);
                        } else {
                            if mode == "cancel" {
                                scope.cancel().unwrap();
                            }
                            let Poll::Ready(Ok(Some(job))) = engine.poll_job(&mut cx) else {
                                panic!("job")
                            };
                            let completion = PageCryptoEngine::process(job);
                            assert!(engine.complete(completion).is_ok());
                            client.poll_budgeted(1).unwrap();
                            assert!(future.as_mut().poll(&mut cx).is_ready());
                            drop(future);
                        }
                    }
                    let mut text = String::new();
                    samples.write(&mut text).unwrap();
                    assert!(text.contains("busy=0"), "{mode}: {text}");
                    assert!(text.contains("send=completed"));
                    assert!(
                        text.contains(if mode == "cached" {
                            "status=cached"
                        } else {
                            "status=unavailable"
                        }),
                        "{mode}: {text}"
                    );
                    if mode == "cached" {
                        assert!(text.contains("crc=000000000000002a"));
                    }
                    if mode == "missing" {
                        assert!(text.contains("MissingKey"));
                    }
                }
            }

            #[test]
            fn shared_worker_encrypt_queue_measurements() {
                shared_worker_queue_measurements(false);
            }

            #[test]
            fn shared_worker_decrypt_queue_measurements() {
                shared_worker_queue_measurements(true);
            }

            fn shared_worker_queue_measurements(decrypt: bool) {
                use crate::security::PageCryptoEngine;
                use std::time::Duration;
                use uring_runtime::environment;
                use uring_runtime::environment::SimulationClock;

                let clock = SimulationClock::new(43);
                let env = clock.environment(0);
                let _env = env.enter();
                let _strict = environment::require_simulated();
                let admission = std::rc::Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
                    crate::test_support::cluster::config(false).limits,
                )));
                let keys = keyring();
                let cache =
                    racer_control_wire::CacheId("00000000-0000-4000-8000-000000000003".into());
                let lease = || {
                    keys.active(&cache, racer_crypto::identity::KeyPurpose::Page)
                        .unwrap()
                };
                let events = measurement_events(decrypt);

                for mode in ["success", "failure", "cancel", "abandon"] {
                    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
                    let scopes = [0, 1].map(|id| {
                        RequestScope::new(
                            crate::model::RequestId([id; 16]),
                            environment::now() + Duration::from_secs(10),
                        )
                        .unwrap()
                    });

                    let (other_io, mut other_engine) =
                        pair(WorkerId(0), 0, NonZeroUsize::new(1).unwrap());
                    let (waiting_io, mut waiting_engine) =
                        pair(WorkerId(1), 0, NonZeroUsize::new(1).unwrap());
                    // Occupy capacity without submitting: the target really parks before
                    // admission, rather than merely being created after a clock advance.
                    let Poll::Ready(Ok(blocker)) = waiting_io.poll_reserve(
                        &mut cx,
                        CryptoId {
                            worker: WorkerId(1),
                            generation: 0,
                            sequence: 0,
                        },
                    ) else {
                        panic!("capacity blocker")
                    };
                    let other = CryptoClient::new(other_io);
                    let waiting = CryptoClient::new(waiting_io);
                    let other_metrics = Metrics::default();
                    let waiting_metrics = Metrics::default();
                    other.set_metrics(other_metrics.clone());
                    waiting.set_metrics(waiting_metrics.clone());
                    let mut future = Some(waiting.execute(
                        measurement_input(
                            &admission,
                            &cache,
                            lease(),
                            &scopes[1],
                            decrypt,
                            mode == "failure",
                        ),
                        lease(),
                        &scopes[1],
                    ));
                    assert!(future.as_mut().unwrap().as_mut().poll(&mut cx).is_pending());
                    assert_eq!(waiting.executor.waiter_counts(), (1, 0));
                    clock.advance(Duration::from_millis(100));
                    assert!(future.as_mut().unwrap().as_mut().poll(&mut cx).is_pending());
                    assert!(waiting_engine.poll_job(&mut cx).is_pending());
                    drop(blocker);
                    assert!(future.as_mut().unwrap().as_mut().poll(&mut cx).is_pending());
                    assert_eq!(waiting.executor.waiter_counts(), (0, 1));

                    let mut other_future = other.execute(
                        measurement_input(&admission, &cache, lease(), &scopes[0], decrypt, false),
                        lease(),
                        &scopes[0],
                    );
                    assert!(other_future.as_mut().poll(&mut cx).is_pending());
                    if mode == "cancel" {
                        scopes[1].cancel().unwrap();
                        assert!(future.as_mut().unwrap().as_mut().poll(&mut cx).is_pending());
                    } else if mode == "abandon" {
                        drop(future.take());
                    }

                    // One execution service thread, manually interleaved: service the
                    // other shard for 7ms before dequeuing the already-submitted target.
                    // Real AEAD has zero virtual cost; set its measured service interval
                    // explicitly, following the single-pair measurement test above.
                    for (engine, worker, service_ms) in [
                        (&mut other_engine, WorkerId(0), 7),
                        (&mut waiting_engine, WorkerId(1), 5),
                    ] {
                        let Poll::Ready(Ok(Some(job))) = engine.poll_job(&mut cx) else {
                            panic!("submitted job")
                        };
                        assert_eq!(job.id().worker, worker);
                        let mut completion = PageCryptoEngine::process(job);
                        assert_eq!(completion.permit.measurement.execution_ns, Some(0));
                        let start = environment::now();
                        clock.advance(Duration::from_millis(service_ms));
                        completion.permit.executed(start);
                        assert!(engine.complete(completion).is_ok());
                    }
                    // Neither the target's own 5ms execution nor this 11ms delay before
                    // I/O reaping belongs in its submit-to-dequeue queue measurement.
                    clock.advance(Duration::from_millis(11));
                    for event in events {
                        assert_eq!(other_metrics.count(event), 0, "before reap {event:?}");
                        assert_eq!(waiting_metrics.count(event), 0, "before reap {event:?}");
                    }
                    other.poll_budgeted(1).unwrap();
                    for event in events {
                        assert_eq!(
                            waiting_metrics.count(event),
                            0,
                            "other shard reaped {event:?}"
                        );
                    }
                    waiting.poll_budgeted(1).unwrap();
                    assert!(matches!(
                        other_future.as_mut().poll(&mut cx),
                        Poll::Ready(Ok(_))
                    ));
                    if let Some(mut future) = future {
                        match (mode, future.as_mut().poll(&mut cx)) {
                            ("success", Poll::Ready(Ok(_)))
                            | ("failure", Poll::Ready(Err(_)))
                            | ("cancel", Poll::Ready(Err(Error::Cancelled))) => {}
                            _ => panic!("unexpected {mode} result"),
                        }
                    }
                    assert_shared_measurements(
                        &other,
                        &waiting,
                        &other_metrics,
                        &waiting_metrics,
                        decrypt,
                        mode,
                    );
                }
            }

            #[test]
            fn duration_saturates_without_host_time_in_dst() {
                use uring_runtime::environment;
                use uring_runtime::environment::SimulationClock;
                let clock = SimulationClock::new(19);
                let env = clock.environment(0);
                let _env = env.enter();
                let _strict = environment::require_simulated();
                let start = environment::now();
                clock.advance(std::time::Duration::from_secs(u64::MAX / 1_000_000_000 + 1));
                assert_eq!(elapsed_ns(start), u64::MAX);
            }

            fn measurement_events(decrypt: bool) -> [Event; 8] {
                if decrypt {
                    [
                        CryptoDecryptStarted,
                        CryptoDecryptSuccess,
                        CryptoDecryptFailure,
                        CryptoDecryptBytes,
                        CryptoDecryptExecutionCount,
                        CryptoDecryptExecutionNs,
                        CryptoDecryptQueueCount,
                        CryptoDecryptQueueNs,
                    ]
                } else {
                    [
                        CryptoEncryptStarted,
                        CryptoEncryptSuccess,
                        CryptoEncryptFailure,
                        CryptoEncryptBytes,
                        CryptoEncryptExecutionCount,
                        CryptoEncryptExecutionNs,
                        CryptoEncryptQueueCount,
                        CryptoEncryptQueueNs,
                    ]
                }
            }

            fn measurement_input(
                admission: &Rc<flow_control::Quotas<AdmissionPolicy>>,
                cache: &CacheId,
                key: KeyLease,
                scope: &RequestScope,
                decrypt: bool,
                corrupt: bool,
            ) -> CryptoInput {
                let mut data = input(admission);
                if decrypt {
                    let (setup, _setup_engine) =
                        pair(WorkerId(2), 0, NonZeroUsize::new(1).unwrap());
                    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
                    let Poll::Ready(Ok(permit)) = setup.poll_reserve(
                        &mut cx,
                        CryptoId {
                            worker: WorkerId(2),
                            generation: 0,
                            sequence: 0,
                        },
                    ) else {
                        panic!("setup permit")
                    };
                    let result = PageCryptoEngine::process(permit.job(data, key, scope.clone()));
                    let CryptoOutcome::Completed(CryptoOutput::Encrypted(plain, mut ciphertext)) =
                        result.outcome
                    else {
                        panic!("setup encryption")
                    };
                    drop(plain);
                    if corrupt {
                        Arc::get_mut(&mut ciphertext.inner).unwrap().bytes[0] ^= 1;
                    }
                    CryptoInput::Decrypt {
                        ciphertext,
                        plaintext: admission
                            .reserve(Some(cache), crate::admission::ResourceClass::Plaintext, 1)
                            .unwrap(),
                    }
                } else {
                    if corrupt && let CryptoInput::Encrypt { page, .. } = &mut data {
                        page.version.object.cache.0 = "wrong".into();
                    }
                    data
                }
            }

            fn assert_shared_measurements(
                other: &CryptoClient,
                waiting: &CryptoClient,
                other_metrics: &Metrics,
                waiting_metrics: &Metrics,
                decrypt: bool,
                mode: &str,
            ) {
                let events = measurement_events(decrypt);
                let opposite_queue = if decrypt {
                    [CryptoEncryptQueueCount, CryptoEncryptQueueNs]
                } else {
                    [CryptoDecryptQueueCount, CryptoDecryptQueueNs]
                };
                let success = u64::from(mode == "success" || mode == "abandon");
                // Repeated drains must not duplicate either shard's observations,
                // even when delivery was canceled or abandoned before execution.
                for _ in 0..3 {
                    other.poll_budgeted(1).unwrap();
                    waiting.poll_budgeted(1).unwrap();
                    for ((event, other_expected), waiting_expected) in events
                        .into_iter()
                        .zip([1, 1, 0, 1, 1, 7_000_000, 1, 0])
                        .zip([1, success, 1 - success, success, 1, 5_000_000, 1, 7_000_000])
                    {
                        assert_eq!(
                            other_metrics.count(event),
                            other_expected,
                            "other {mode} {event:?}"
                        );
                        assert_eq!(
                            waiting_metrics.count(event),
                            waiting_expected,
                            "waiting {mode} {event:?}"
                        );
                    }
                    for event in opposite_queue {
                        assert_eq!(other_metrics.count(event), 0);
                        assert_eq!(waiting_metrics.count(event), 0);
                    }
                    assert_eq!(other.outstanding(), 0);
                    assert_eq!(waiting.outstanding(), 0);
                }
            }

            struct MeasurementInputs {
                admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
                pool: BufferPool,
                page: PageId,
                source: Vec<u8>,
                descriptor: PageEnvelope,
                encrypted: Vec<u8>,
                size: usize,
            }

            impl MeasurementInputs {
                fn new(size: usize, operation: &str) -> Self {
                    let mut limits = crate::test_support::cluster::config(false).limits;
                    limits.plaintext_bytes = NonZeroUsize::new(512 * 1024 * 1024).unwrap();
                    limits.ciphertext_bytes = limits.plaintext_bytes;
                    let admission =
                        Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(limits)));
                    let pool = BufferPool::new(admission.clone());
                    let page = page();
                    let source = vec![7u8; size];
                    let descriptor = envelope(size);
                    let mut encrypted = vec![0; size + TAG_LEN];
                    racer_crypto::seal(
                        &[7; 32],
                        &descriptor.nonce.0,
                        &page_aad(&descriptor).unwrap(),
                        &source,
                        &mut encrypted,
                    )
                    .unwrap();
                    if operation == "bad_tag" {
                        encrypted[size] ^= 1;
                    }
                    Self {
                        admission,
                        pool,
                        page,
                        source,
                        descriptor,
                        encrypted,
                        size,
                    }
                }

                fn input(&self, operation: &str) -> CryptoInput {
                    let Self {
                        admission,
                        pool,
                        page,
                        source,
                        descriptor,
                        encrypted,
                        size,
                    } = self;
                    let size = *size;
                    let cache = &page.version.object.cache;
                    if operation == "encrypt" {
                        let mut plain = pool
                            .plaintext(
                                admission
                                    .reserve(Some(cache), ResourceClass::Plaintext, size)
                                    .unwrap(),
                                size,
                            )
                            .unwrap();
                        plain.bytes_mut().unwrap().copy_from_slice(source);
                        CryptoInput::Encrypt {
                            page: page.clone(),
                            plaintext: plain,
                            ciphertext: admission
                                .reserve(Some(cache), ResourceClass::Ciphertext, size + 16)
                                .unwrap(),
                        }
                    } else {
                        // Fresh received allocation/checksum state, as on disk/peer ingress.
                        let bytes = encrypted;
                        let ciphertext = pool
                            .ciphertext(
                                admission
                                    .reserve(Some(cache), ResourceClass::Ciphertext, bytes.len())
                                    .unwrap(),
                                descriptor.clone(),
                                bytes.clone(),
                            )
                            .unwrap();
                        CryptoInput::Decrypt {
                            ciphertext,
                            plaintext: admission
                                .reserve(Some(cache), ResourceClass::Plaintext, size)
                                .unwrap(),
                        }
                    }
                }
            }
        }

        #[cfg(test)]
        mod channel_tests {
            #[cfg(test)]
            mod tests {
                use std::task::Context;
                use std::task::Poll;
                use uring_runtime::Error;
                use uring_runtime::channel::*;

                #[test]
                fn adapter_maps_errors_without_losing_command_ownership() {
                    assert!(matches!(bounded::<u8>(0), Err(Error::InvalidConfiguration)));
                    let (sender, mut receiver) = bounded(1).unwrap();
                    assert!(sender.try_send(String::from("first")).is_ok());
                    let failure = sender.try_send(String::from("second")).err().unwrap();
                    assert_eq!(failure.error, Error::Overloaded);
                    assert_eq!(failure.command, "second");
                    assert_eq!(receiver.receive().unwrap().as_deref(), Some("first"));
                    drop(receiver);
                    let failure = sender.try_send(failure.command).err().unwrap();
                    assert_eq!(failure.error, Error::Unavailable);
                    assert_eq!(failure.command, "second");
                    assert_eq!(
                        sender
                            .poll_ready(&mut Context::from_waker(futures::task::noop_waker_ref())),
                        Poll::Ready(Err(Error::Unavailable))
                    );
                    assert!(sender.discard_closed());
                }
            }
        }
        #[cfg(test)]
        mod tests {
            use super::*;
            use crate::test_support::WakeCounter;

            struct Fixture {
                admission: std::rc::Rc<flow_control::Quotas<AdmissionPolicy>>,
                client: CryptoClient,
                engine: CryptoPort,
                scope: RequestScope,
            }

            impl Fixture {
                fn new() -> Self {
                    let admission = std::rc::Rc::new(flow_control::Quotas::new(
                        AdmissionPolicy::new(crate::test_support::cluster::config(false).limits),
                    ));
                    let (io, engine) = pair(WorkerId(0), 0, NonZeroUsize::new(1).unwrap());
                    Self {
                        admission,
                        client: CryptoClient::new(io),
                        engine,
                        scope: RequestScope::new(
                            crate::model::RequestId([0; 16]),
                            uring_runtime::environment::now() + std::time::Duration::from_secs(5),
                        )
                        .unwrap(),
                    }
                }
            }

            pub(super) fn input(
                admission: &std::rc::Rc<flow_control::Quotas<AdmissionPolicy>>,
            ) -> CryptoInput {
                use crate::admission::ResourceClass;
                use crate::memory::BufferPool;
                use crate::model::*;
                let cache = CacheId("00000000-0000-4000-8000-000000000003".into());
                CryptoInput::Encrypt {
                    page: PageId {
                        version: ObjectVersion {
                            object: ObjectId {
                                cache: cache.clone(),
                                key: CacheKey([0; 32]),
                            },
                            etag: StrongEtag::test_value("v1"),
                        },
                        number: PageNumber(0),
                    },
                    plaintext: BufferPool::new(admission.clone())
                        .plaintext(
                            admission
                                .reserve(Some(&cache), ResourceClass::Plaintext, 1)
                                .unwrap(),
                            1,
                        )
                        .unwrap(),
                    ciphertext: admission
                        .reserve(Some(&cache), ResourceClass::Ciphertext, 17)
                        .unwrap(),
                }
            }

            #[test]
            fn engine_loss_reclaims_queued_owners_and_unblocks_drain() {
                use crate::admission::ResourceClass;
                let Fixture {
                    admission,
                    client,
                    engine,
                    scope,
                } = Fixture::new();
                let mut future = client.execute(input(&admission), key(), &scope);
                let mut cx = Context::from_waker(futures::task::noop_waker_ref());
                assert!(future.as_mut().poll(&mut cx).is_pending());
                drop(engine);
                client.poll_budgeted(1).unwrap();
                assert_eq!(client.outstanding(), 0);
                assert_eq!(admission.used(ResourceClass::Plaintext), 0);
                assert!(matches!(future.as_mut().poll(&mut cx), Poll::Ready(Err(_))));
                assert!(client.drain(&scope).as_mut().poll(&mut cx).is_ready());
            }

            #[test]
            fn accepted_cancellation_cannot_return_before_completion_consumption() {
                use crate::admission::ResourceClass;
                let Fixture {
                    admission,
                    client,
                    mut engine,
                    scope,
                } = Fixture::new();
                let mut future = client.execute(input(&admission), key(), &scope);
                let mut cx = Context::from_waker(futures::task::noop_waker_ref());
                assert!(future.as_mut().poll(&mut cx).is_pending());
                scope.cancel().unwrap();
                for _ in 0..4 {
                    client.poll_budgeted(1).unwrap();
                    assert!(future.as_mut().poll(&mut cx).is_pending());
                }
                assert_eq!(client.outstanding(), 1);
                assert_eq!(admission.used(ResourceClass::Plaintext), 1);
                let job = match engine.poll_job(&mut cx) {
                    Poll::Ready(Ok(Some(job))) => job,
                    _ => panic!("job"),
                };
                let completion = crate::security::PageCryptoEngine::process(job);
                assert!(engine.complete(completion).is_ok());
                assert!(
                    future.as_mut().poll(&mut cx).is_pending(),
                    "queued completion is not consumed"
                );
                client.poll_budgeted(1).unwrap();
                assert!(matches!(
                    future.as_mut().poll(&mut cx),
                    Poll::Ready(Err(Error::Cancelled))
                ));
                assert_eq!(client.outstanding(), 0);
                assert_eq!(admission.used(ResourceClass::Plaintext), 0);
            }

            #[test]
            fn accepted_deadline_expiry_waits_for_engine_completion() {
                let clock = uring_runtime::environment::SimulationClock::new(91);
                let _environment = clock.environment(0).enter();
                let Fixture {
                    admission,
                    client,
                    mut engine,
                    scope,
                } = Fixture::new();
                let key = key();
                let mut future = client.execute(input(&admission), key, &scope);
                let mut cx = Context::from_waker(futures::task::noop_waker_ref());
                assert!(future.as_mut().poll(&mut cx).is_pending());
                clock.advance(std::time::Duration::from_secs(5));
                assert!(future.as_mut().poll(&mut cx).is_pending());
                let job = match engine.poll_job(&mut cx) {
                    Poll::Ready(Ok(Some(job))) => job,
                    _ => panic!("job"),
                };
                let completion = crate::security::PageCryptoEngine::process(job);
                assert!(engine.complete(completion).is_ok());
                client.poll_budgeted(1).unwrap();
                assert!(matches!(
                    future.as_mut().poll(&mut cx),
                    Poll::Ready(Err(Error::DeadlineExceeded))
                ));
                assert_eq!(client.outstanding(), 0);
            }

            #[test]
            fn abandoned_task_cannot_replace_worker_completion_wake() {
                let Fixture {
                    admission,
                    client,
                    mut engine,
                    scope,
                } = Fixture::new();
                let driver = Arc::new(WakeCounter::default());
                client.register_driver(&Waker::from(driver.clone()));
                let mut future = client.execute(input(&admission), key(), &scope);
                let mut cx = Context::from_waker(futures::task::noop_waker_ref());
                assert!(future.as_mut().poll(&mut cx).is_pending());
                drop(future);
                let job = match engine.poll_job(&mut cx) {
                    Poll::Ready(Ok(Some(job))) => job,
                    _ => panic!("job"),
                };
                let CryptoJob {
                    permit,
                    input,
                    key,
                    scope: _,
                } = job;
                assert!(
                    engine
                        .complete(CryptoCompletion {
                            permit,
                            outcome: CryptoOutcome::Failed {
                                input,
                                error: Error::Cancelled
                            },
                            _key: key,
                        })
                        .is_ok()
                );
                assert_eq!(driver.count(), 1);
                client.poll_budgeted(1).unwrap();
                assert_eq!(client.outstanding(), 0);
            }

            #[test]
            fn drain_scope_cancellation_wakes_even_with_unconsumed_result() {
                use crate::model::RequestId;
                let Fixture {
                    admission,
                    client,
                    mut engine,
                    scope,
                } = Fixture::new();
                let mut future = client.execute(input(&admission), key(), &scope);
                let mut cx = Context::from_waker(futures::task::noop_waker_ref());
                assert!(future.as_mut().poll(&mut cx).is_pending());
                let job = match engine.poll_job(&mut cx) {
                    Poll::Ready(Ok(Some(job))) => job,
                    _ => panic!("job"),
                };
                let CryptoJob {
                    permit,
                    input,
                    key,
                    scope: _,
                } = job;
                assert!(
                    engine
                        .complete(CryptoCompletion {
                            permit,
                            outcome: CryptoOutcome::Failed {
                                input,
                                error: Error::Cancelled
                            },
                            _key: key,
                        })
                        .is_ok()
                );
                client.poll_budgeted(1).unwrap();
                let drain_scope = RequestScope::new(RequestId([1; 16]), scope.deadline.0).unwrap();
                let count = Arc::new(WakeCounter::default());
                let waker = Waker::from(count.clone());
                let mut drain_cx = Context::from_waker(&waker);
                let mut drain = client.drain(&drain_scope);
                assert!(drain.as_mut().poll(&mut drain_cx).is_pending());
                drain_scope.cancel().unwrap();
                assert!(count.count() > 0);
                assert!(matches!(
                    drain.as_mut().poll(&mut drain_cx),
                    Poll::Ready(Ok(()))
                ));
                assert!(matches!(
                    future.as_mut().poll(&mut cx),
                    Poll::Ready(Err(Error::Cancelled))
                ));
            }

            #[test]
            fn capacity_tracks_reserved_owners_and_rejects_reused_identity() {
                let (io, _engine) = pair(WorkerId(1), 4, NonZeroUsize::new(1).unwrap());
                let mut cx = Context::from_waker(futures::task::noop_waker_ref());
                let first = CryptoId {
                    worker: WorkerId(1),
                    generation: 4,
                    sequence: 1,
                };
                let permit = match io.poll_reserve(&mut cx, first) {
                    Poll::Ready(Ok(permit)) => permit,
                    _ => panic!("first permit"),
                };
                let second = CryptoId {
                    sequence: 2,
                    ..first
                };
                assert!(io.poll_reserve(&mut cx, second).is_pending());
                assert!(matches!(
                    io.poll_reserve(&mut cx, first),
                    Poll::Ready(Err(Error::StaleFlight))
                ));
                drop(permit);
                assert!(matches!(
                    io.poll_reserve(&mut cx, second),
                    Poll::Ready(Ok(_))
                ));
                io.close_submissions().unwrap();
                assert!(matches!(
                    io.poll_reserve(
                        &mut cx,
                        CryptoId {
                            sequence: 3,
                            ..first
                        }
                    ),
                    Poll::Ready(Err(Error::Unavailable))
                ));
            }

            #[test]
            fn wrong_pair_generation_never_debits_capacity() {
                let (io, _engine) = pair(WorkerId(1), 4, NonZeroUsize::new(1).unwrap());
                let mut cx = Context::from_waker(futures::task::noop_waker_ref());
                for (worker, generation) in [(WorkerId(2), 4), (WorkerId(1), 3)] {
                    assert!(matches!(
                        io.poll_reserve(
                            &mut cx,
                            CryptoId {
                                worker,
                                generation,
                                sequence: 1
                            }
                        ),
                        Poll::Ready(Err(Error::StaleFlight))
                    ));
                }
                assert_eq!(io.queue.outstanding(), 0);
            }

            #[test]
            fn invalid_queue_capacity_is_a_startup_error_not_a_panic() {
                assert!(matches!(
                    try_pair(WorkerId(0), 0, NonZeroUsize::new(usize::MAX).unwrap()),
                    Err(Error::InvalidConfiguration)
                ));
            }

            pub(super) fn key() -> KeyLease {
                keyring()
                    .active(
                        &racer_control_wire::CacheId("00000000-0000-4000-8000-000000000003".into()),
                        racer_crypto::identity::KeyPurpose::Page,
                    )
                    .unwrap()
            }

            pub(super) fn keyring() -> racer_crypto::identity::Keyring {
                use racer_control_wire::CacheId;
                use racer_control_wire::ClusterId;
                use racer_control_wire::NodeId;
                use racer_control_wire::*;
                use racer_crypto::identity::KeyEpochs;
                use racer_crypto::identity::Keyring;
                let ca_key = rcgen::KeyPair::generate_for(&rcgen::PKCS_ED25519).unwrap();
                let mut params = rcgen::CertificateParams::default();
                params.is_ca = rcgen::IsCa::Ca(rcgen::BasicConstraints::Unconstrained);
                let ca = params.self_signed(&ca_key).unwrap();
                let keys = Keyring::new(
                    ClusterId("00000000-0000-4000-8000-000000000001".into()),
                    NodeId("00000000-0000-4000-8000-000000000002".into()),
                    Arc::new(KeyEpochs::default()),
                );
                let cache = CacheId("00000000-0000-4000-8000-000000000003".into());
                keys.install(KeyringBundle {
                    schema_version: 1,
                    cluster: ClusterId("00000000-0000-4000-8000-000000000001".into()),
                    generation: BundleGeneration(1),
                    peer_trust_roots: vec![ca.der().to_vec()],
                    cache_keys: vec![CacheEncryptionKey::new(
                        CacheKeyRef {
                            cache: cache.clone(),
                            id: crate::model::key_id_from_generation(1, 1).unwrap(),
                            purpose: CacheKeyPurpose::Page,
                        },
                        CacheKeyState::Active,
                        zeroize::Zeroizing::new([7; 32]),
                    )],
                })
                .unwrap();
                keys
            }

            #[test]
            fn abandoned_future_retains_buffers_key_and_permit_until_reaped() {
                use crate::admission::ResourceClass;
                use crate::memory::BufferPool;
                use crate::model::*;
                use std::rc::Rc;
                use std::time::Duration;
                use std::time::Instant;
                let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
                    crate::test_support::cluster::config(false).limits,
                )));
                let cache = CacheId("cache".into());
                let pool = BufferPool::new(admission.clone());
                let plaintext = pool
                    .plaintext(
                        admission
                            .reserve(Some(&cache), ResourceClass::Plaintext, 3)
                            .unwrap(),
                        3,
                    )
                    .unwrap();
                let input = CryptoInput::Encrypt {
                    page: PageId {
                        version: ObjectVersion {
                            object: ObjectId {
                                cache: cache.clone(),
                                key: CacheKey([0; 32]),
                            },
                            etag: StrongEtag::test_value("v1"),
                        },
                        number: PageNumber(0),
                    },
                    plaintext,
                    ciphertext: admission
                        .reserve(Some(&cache), ResourceClass::Ciphertext, 19)
                        .unwrap(),
                };
                let (io, mut engine) = pair(WorkerId(0), 1, NonZeroUsize::new(1).unwrap());
                let client = CryptoClient::new(io);
                let scope =
                    RequestScope::new(RequestId([0; 16]), Instant::now() + Duration::from_secs(5))
                        .unwrap();
                let mut operation = client.execute(input, key(), &scope);
                let mut cx = Context::from_waker(futures::task::noop_waker_ref());
                struct Count(std::sync::atomic::AtomicUsize);
                impl std::task::Wake for Count {
                    fn wake(self: Arc<Self>) {
                        self.0.fetch_add(1, Ordering::Relaxed);
                    }
                }
                let count = Arc::new(Count(std::sync::atomic::AtomicUsize::new(0)));
                let driver = Waker::from(count.clone());
                client.register_driver(&driver);
                assert!(operation.as_mut().poll(&mut cx).is_pending());
                drop(operation);
                assert_eq!(client.outstanding(), 1);
                assert_eq!(admission.used(ResourceClass::Plaintext), 3);
                let job = match engine.poll_job(&mut cx) {
                    Poll::Ready(Ok(Some(job))) => job,
                    _ => panic!("accepted job"),
                };
                let CryptoJob {
                    permit,
                    input,
                    key,
                    scope: _,
                } = job;
                assert!(
                    engine
                        .complete(CryptoCompletion {
                            permit,
                            outcome: CryptoOutcome::Failed {
                                input,
                                error: Error::Cancelled
                            },
                            _key: key,
                        })
                        .is_ok()
                );
                assert_eq!(client.outstanding(), 1);
                assert_eq!(admission.used(ResourceClass::Plaintext), 3);
                client.poll_budgeted(1).unwrap();
                assert!(
                    count.0.load(Ordering::Relaxed) > 0,
                    "abandoned operation must not steal driver wake"
                );
                assert_eq!(client.outstanding(), 0);
                assert_eq!(admission.used(ResourceClass::Plaintext), 0);
                assert_eq!(admission.used(ResourceClass::Ciphertext), 0);
            }

            #[test]
            fn engine_drop_reclaims_queued_jobs_and_drain_finishes() {
                use crate::admission::ResourceClass;
                use crate::memory::BufferPool;
                use crate::model::*;
                use std::rc::Rc;
                use std::time::Duration;
                use std::time::Instant;
                let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
                    crate::test_support::cluster::config(false).limits,
                )));
                let cache = CacheId("cache".into());
                let plaintext = BufferPool::new(admission.clone())
                    .plaintext(
                        admission
                            .reserve(Some(&cache), ResourceClass::Plaintext, 1)
                            .unwrap(),
                        1,
                    )
                    .unwrap();
                let page = PageId {
                    version: ObjectVersion {
                        object: ObjectId {
                            cache: cache.clone(),
                            key: CacheKey([0; 32]),
                        },
                        etag: StrongEtag::test_value("v1"),
                    },
                    number: PageNumber(0),
                };
                let ciphertext = admission
                    .reserve(Some(&cache), ResourceClass::Ciphertext, 17)
                    .unwrap();
                let (io, engine) = pair(WorkerId(0), 0, NonZeroUsize::new(1).unwrap());
                let client = CryptoClient::new(io);
                let scope =
                    RequestScope::new(RequestId([0; 16]), Instant::now() + Duration::from_secs(5))
                        .unwrap();
                let mut operation = client.execute(
                    CryptoInput::Encrypt {
                        page,
                        plaintext,
                        ciphertext,
                    },
                    key(),
                    &scope,
                );
                let mut cx = Context::from_waker(futures::task::noop_waker_ref());
                assert!(operation.as_mut().poll(&mut cx).is_pending());
                drop(engine);
                client.poll_budgeted(1).unwrap();
                assert!(matches!(
                    operation.as_mut().poll(&mut cx),
                    Poll::Ready(Err(_))
                ));
                drop(operation);
                assert_eq!(client.outstanding(), 0);
                assert_eq!(admission.used(ResourceClass::Plaintext), 0);
                assert!(client.drain(&scope).as_mut().poll(&mut cx).is_ready());
            }

            #[test]
            fn original_scope_failure_is_returned_without_submission() {
                use crate::admission::ResourceClass;
                use crate::memory::BufferPool;
                use crate::model::*;
                use std::rc::Rc;
                use std::time::Instant;
                let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
                    crate::test_support::cluster::config(false).limits,
                )));
                let cache = CacheId("cache".into());
                let plaintext = BufferPool::new(admission.clone())
                    .plaintext(
                        admission
                            .reserve(Some(&cache), ResourceClass::Plaintext, 1)
                            .unwrap(),
                        1,
                    )
                    .unwrap();
                let ciphertext = admission
                    .reserve(Some(&cache), ResourceClass::Ciphertext, 17)
                    .unwrap();
                let page = PageId {
                    version: ObjectVersion {
                        object: ObjectId {
                            cache,
                            key: CacheKey([0; 32]),
                        },
                        etag: StrongEtag::test_value("v1"),
                    },
                    number: PageNumber(0),
                };
                let (io, mut engine) = pair(WorkerId(0), 0, NonZeroUsize::new(1).unwrap());
                let client = CryptoClient::new(io);
                let scope = RequestScope::new(RequestId([0; 16]), Instant::now()).unwrap();
                let result = futures::executor::block_on(client.execute(
                    CryptoInput::Encrypt {
                        page,
                        plaintext,
                        ciphertext,
                    },
                    key(),
                    &scope,
                ));
                assert!(matches!(result, Err(Error::DeadlineExceeded)));
                assert_eq!(client.outstanding(), 0);
                assert_eq!(admission.used(ResourceClass::Plaintext), 0);
                assert!(
                    engine
                        .poll_job(&mut Context::from_waker(futures::task::noop_waker_ref()))
                        .is_pending()
                );
            }

            #[test]
            fn only_owned_messages_and_endpoints_cross_threads() {
                fn send<T: Send + 'static>() {}
                send::<CryptoInput>();
                send::<CryptoOutput>();
                send::<CryptoJob>();
                send::<CryptoCompletion>();
                send::<CryptoPermit>();
                send::<IoCryptoPort>();
                send::<CryptoPort>();
            }

            #[test]
            fn endpoints_enforce_pair_capacity_generation_and_close() {
                let (io, engine) = pair(WorkerId(7), 12, NonZeroUsize::new(3).unwrap());
                let mut cx = Context::from_waker(futures::task::noop_waker_ref());
                for (worker, generation) in [(WorkerId(8), 12), (WorkerId(7), 13)] {
                    assert!(matches!(
                        io.poll_reserve(
                            &mut cx,
                            CryptoId {
                                worker,
                                generation,
                                sequence: 0
                            }
                        ),
                        Poll::Ready(Err(Error::StaleFlight))
                    ));
                }
                let id = |sequence| CryptoId {
                    worker: WorkerId(7),
                    generation: 12,
                    sequence,
                };
                let mut permits = Vec::new();
                for sequence in 0..3 {
                    let Poll::Ready(Ok(permit)) = io.poll_reserve(&mut cx, id(sequence)) else {
                        panic!("available permit")
                    };
                    permits.push(permit);
                }
                assert!(io.poll_reserve(&mut cx, id(3)).is_pending());
                drop(permits.pop());
                assert!(matches!(
                    io.poll_reserve(&mut cx, id(3)),
                    Poll::Ready(Ok(_))
                ));
                drop(engine);
                assert!(matches!(
                    io.poll_reserve(&mut cx, id(4)),
                    Poll::Ready(Err(Error::Unavailable))
                ));
            }
        }
    }
    fn envelope() -> PageEnvelope {
        PageEnvelope {
            page: PageId {
                version: ObjectVersion {
                    object: ObjectId {
                        cache: CacheId(crate::test_support::security::CACHE.into()),
                        key: CacheKey([3; 32]),
                    },
                    etag: StrongEtag::parse(b"\"v1\"").unwrap(),
                },
                number: PageNumber(7),
            },
            key_id: crate::model::key_id_from_generation(1, 1).unwrap(),
            nonce: Nonce([2; 24]),
            plaintext_length: 5,
            ciphertext_length: 21,
        }
    }
    #[test]
    fn page_aad_is_domain_separated_and_binds_every_descriptor_field() {
        let original = envelope();
        let aad = page_aad(&original).unwrap();
        assert!(aad.starts_with(b"racer/page/aead/v1\0\0\0\0\x24"));
        let mut bytes = vec![0; 5 + TAG_LEN];
        racer_crypto::seal(&[7; 32], &original.nonce.0, &aad, b"hello", &mut bytes).unwrap();
        for index in 0..8 {
            let mut changed = original.clone();
            match index {
                0 => {
                    changed.page.version.object.cache =
                        CacheId(crate::test_support::security::NODE.into())
                }
                1 => changed.page.version.object.key.0[0] ^= 1,
                2 => changed.page.version.etag = StrongEtag::parse(b"\"v2\"").unwrap(),
                3 => changed.page.number.0 += 1,
                4 => changed.key_id.0[0] ^= 1,
                5 => changed.nonce.0[0] ^= 1,
                6 => {
                    changed.plaintext_length += 1;
                    changed.ciphertext_length += 1;
                }
                _ => changed.page.version.object.cache.0.replace_range(..1, "4"),
            }
            let mut tampered = vec![0; 5];
            assert!(
                racer_crypto::open(
                    &[7; 32],
                    &changed.nonce.0,
                    &page_aad(&changed).unwrap(),
                    &bytes,
                    &mut tampered
                )
                .is_err()
            );
        }
        let mut corrupted = bytes.clone();
        corrupted[0] ^= 1;
        let mut output = vec![0; 5];
        assert!(
            racer_crypto::open(&[7; 32], &original.nonce.0, &aad, &corrupted, &mut output).is_err()
        );
        racer_crypto::open(&[7; 32], &original.nonce.0, &aad, &bytes, &mut output).unwrap();
        assert_eq!(output, b"hello");
        let mut malformed = original;
        malformed.ciphertext_length += 1;
        assert!(page_aad(&malformed).is_err());
    }
    #[test]
    fn libsodium_independent_known_answer() {
        // Independently generated with libsodium 1.0.18
        // crypto_aead_xchacha20poly1305_ietf_encrypt, not this Rust implementation.
        let mut descriptor = envelope();
        descriptor.key_id = KeyId([1; 16]); // Frozen independent AAD vector, not installed.
        let mut bytes = vec![0; 5 + TAG_LEN];
        racer_crypto::seal(
            &[7; 32],
            &[2; 24],
            &page_aad(&descriptor).unwrap(),
            b"hello",
            &mut bytes,
        )
        .unwrap();
        assert_eq!(
            bytes,
            [
                0xb2, 0xd4, 0x6c, 0x90, 0xe7, 0x29, 0x99, 0x95, 0x55, 0x73, 0x9c, 0xaf, 0x01, 0xa6,
                0xa9, 0x89, 0xcf, 0xfb, 0x34, 0xc3, 0x6f
            ]
        );
    }
    #[test]
    fn libsodium_boundary_and_full_page_detached_vectors() {
        use crate::admission::AdmissionPolicy;
        use crate::memory::BufferPool;
        use crate::security::CryptoId;
        use crate::security::pair;
        use sha2::Digest;
        use sha2::Sha256;

        // SHA-256 of the complete ciphertext plus detached tag, independently
        // generated with libsodium 1.0.18. Reproduce with aead_vectors.py.
        // Hashes avoid checking in 32 MiB of near-full/full-page ciphertext.
        let vectors = [
            (
                1,
                "e458e0dc619b62fd62ccfd6b0a64edd478d17dcbe2a90ae5cf5a1236c7e66b42",
            ),
            (
                15,
                "305a5c76814aa3a7fa7ffd6013674c1569d48050214fd99046b62835558522f7",
            ),
            (
                16,
                "fd224886bdc4dce663d1cfe23f152b82bf92ec4c3585e459284172817366bfe8",
            ),
            (
                17,
                "e8a89e5c8b5672afa89102a03e8333065071e9223c08f9829b0211862ed827fb",
            ),
            (
                63,
                "5732225fec76e8bbe0af47bfd2b89dc4d6b10c76a038ef57cf3dd3f9b26cd0f3",
            ),
            (
                64,
                "9a74018a50d140c758cbac254d2400f54268efef40ee729109a65b2430a60541",
            ),
            (
                65,
                "581818d82f394c7c27cd07dcfecf8aaa7281291a0a8ba27226c07a5b8b94b1ff",
            ),
            (
                255,
                "34061699d97d185ebef4b17f329ee4a94b8e667cd9f0ea86952f76a12337ab35",
            ),
            (
                256,
                "3691e12ccc85a3ee991d554984da2d1fbcc99f928994986838eea83e5def0b36",
            ),
            (
                257,
                "d31b2dcd6ee4646e35b0347efe0ff9c277240b821dce4813e1b6e2222a908646",
            ),
            (
                16777215,
                "19eb6c4a424b572bc65b6200a72e29c0fcfbedfc74ad522ba021cc150f1606d6",
            ),
            (
                16777216,
                "b8a08e70bf42b3eb50de6a5e9f852c9a664bfa8a4a497aa2a393f5ccc8ad74e0",
            ),
        ];
        let keys = crate::test_support::security::keys();
        let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
            crate::test_support::cluster::config(false).limits,
        )));
        let pool = BufferPool::new(admission.clone());
        let (io, _port) = pair(WorkerId(0), 1, std::num::NonZeroUsize::new(1).unwrap());
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        for (sequence, (length, expected)) in vectors.into_iter().enumerate() {
            let mut descriptor = envelope();
            descriptor.key_id = KeyId([1; 16]); // Frozen independent AAD vector.
            descriptor.plaintext_length = length as u32;
            descriptor.ciphertext_length = length as u32 + 16;
            let cache = &descriptor.page.version.object.cache;
            let raw: Vec<u8> = (0..length)
                .map(|i| (i as u8).wrapping_mul(31).wrapping_add(7))
                .collect();
            let mut encrypted = vec![0; length + 16];
            racer_crypto::seal(
                &[7; 32],
                &descriptor.nonce.0,
                &page_aad(&descriptor).unwrap(),
                &raw,
                &mut encrypted,
            )
            .unwrap();
            assert_eq!(
                format!("{:x}", Sha256::digest(&encrypted)),
                expected,
                "length={length}"
            );
            // Exercise the engine with an installable current key ID, while
            // retaining the independent historical AAD known-answer assertion.
            descriptor.key_id = keys.active(cache, KeyPurpose::Page).unwrap().id();
            racer_crypto::seal(
                &[7; 32],
                &descriptor.nonce.0,
                &page_aad(&descriptor).unwrap(),
                &raw,
                &mut encrypted,
            )
            .unwrap();
            let current_digest = Sha256::digest(&encrypted);
            let original = pool
                .ciphertext(
                    admission
                        .reserve(Some(cache), ResourceClass::Ciphertext, length + 16)
                        .unwrap(),
                    descriptor.clone(),
                    encrypted,
                )
                .unwrap();
            let pointer = original.bytes().as_ptr();
            let Poll::Ready(Ok(permit)) = io.poll_reserve(
                &mut cx,
                CryptoId {
                    worker: WorkerId(0),
                    generation: 1,
                    sequence: sequence as u64 + 1,
                },
            ) else {
                panic!("reserve")
            };
            let completion = PageCryptoEngine::process(
                permit.job(
                    CryptoInput::Decrypt {
                        ciphertext: original,
                        plaintext: admission
                            .reserve(Some(cache), ResourceClass::Plaintext, length)
                            .unwrap(),
                    },
                    keys.active(cache, KeyPurpose::Page).unwrap(),
                    RequestScope::new(
                        RequestId([1; 16]),
                        std::time::Instant::now() + std::time::Duration::from_secs(10),
                    )
                    .unwrap(),
                ),
            );
            let CryptoOutcome::Completed(CryptoOutput::Decrypted(clear, original)) =
                &completion.outcome
            else {
                panic!("decrypt length={length}")
            };
            assert_eq!(clear.bytes(), raw);
            assert_eq!(original.bytes().as_ptr(), pointer);
            assert_eq!(Sha256::digest(original.bytes()), current_digest);
            assert_eq!(admission.used(ResourceClass::Plaintext), length);
            assert_eq!(admission.used(ResourceClass::Ciphertext), length + 16);
            drop(completion);
            admission.reclaim_buffers();
            assert_eq!(admission.used(ResourceClass::Plaintext), 0);
            assert_eq!(admission.used(ResourceClass::Ciphertext), 0);
        }
    }
    #[test]
    fn detached_authentication_failures_do_not_write_output_or_mutate_input() {
        let descriptor = envelope();
        let aad = page_aad(&descriptor).unwrap();
        let mut encrypted = [0; 5 + TAG_LEN];
        racer_crypto::seal(
            &[7; 32],
            &descriptor.nonce.0,
            &aad,
            b"hello",
            &mut encrypted,
        )
        .unwrap();
        for fault in 0..4 {
            let mut nonce = descriptor.nonce.0;
            let mut aad = aad.clone();
            let mut input = encrypted;
            match fault {
                0 => nonce[0] ^= 1,
                1 => aad[0] ^= 1,
                2 => input[0] ^= 1,
                _ => input[5] ^= 1,
            }
            let retained = input;
            // A nonzero sentinel proves authentication precedes any output write,
            // rather than merely observing the zero-initialized production buffer.
            let mut output = [0xa5; 5];
            assert!(racer_crypto::open(&[7; 32], &nonce, &aad, &input, &mut output).is_err());
            assert_eq!(output, [0xa5; 5]);
            assert_eq!(input, retained);
        }
    }
    #[test]
    fn random_nonces_do_not_repeat() {
        let mut seen = std::collections::HashSet::new();
        for _ in 0..1024 {
            assert!(seen.insert(fresh_nonce().unwrap().0));
        }
    }
    #[test]
    fn engine_preserves_failed_inputs_and_completion_capacity() {
        use crate::admission::AdmissionPolicy;
        use crate::memory::BufferPool;
        use crate::security::CryptoId;
        use crate::security::pair;
        let keys = crate::test_support::security::keys();
        let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
            crate::test_support::cluster::config(false).limits,
        )));
        let pool = BufferPool::new(admission.clone());
        let descriptor = envelope();
        let cache = &descriptor.page.version.object.cache;
        let (io, port) = pair(WorkerId(0), 1, std::num::NonZeroUsize::new(1).unwrap());
        let mut engine = PageCryptoEngine::new(CryptoRuntime { port });
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        for sequence in 1..=6 {
            let canceled = sequence == 2;
            let corrupt = sequence >= 3;
            let mut descriptor = descriptor.clone();
            let mut bytes = vec![0; 5 + TAG_LEN];
            racer_crypto::seal(
                &[7; 32],
                &descriptor.nonce.0,
                &page_aad(&descriptor).unwrap(),
                b"hello",
                &mut bytes,
            )
            .unwrap();
            match sequence {
                3 => bytes[0] ^= 1,
                4 => descriptor.nonce.0[0] ^= 1,
                5 => descriptor.page.number.0 += 1,
                6 => bytes[20] ^= 1,
                _ => {}
            }
            let cipher = pool
                .ciphertext(
                    admission
                        .reserve(Some(cache), ResourceClass::Ciphertext, 21)
                        .unwrap(),
                    descriptor.clone(),
                    bytes.clone(),
                )
                .unwrap();
            let original_pointer = cipher.bytes().as_ptr();
            let input = CryptoInput::Decrypt {
                ciphertext: cipher,
                plaintext: admission
                    .reserve(Some(cache), ResourceClass::Plaintext, 5)
                    .unwrap(),
            };
            let scope = RequestScope::new(
                RequestId([1; 16]),
                std::time::Instant::now() + std::time::Duration::from_secs(10),
            )
            .unwrap();
            if canceled {
                scope.cancel().unwrap();
            }
            let id = CryptoId {
                worker: WorkerId(0),
                generation: 1,
                sequence,
            };
            let Poll::Ready(Ok(permit)) = io.poll_reserve(&mut cx, id) else {
                panic!("reserve");
            };
            assert!(
                io.try_submit(permit.job(
                    input,
                    keys.active(cache, KeyPurpose::Page).unwrap(),
                    scope
                ))
                .is_ok()
            );
            engine
                .poll_budgeted(&mut Context::from_waker(futures::task::noop_waker_ref()), 1)
                .unwrap();
            let next = CryptoId {
                sequence: sequence + 1,
                ..id
            };
            assert!(io.poll_reserve(&mut cx, next).is_pending());
            let Poll::Ready(Ok(Some(completion))) = io.poll_completion(&mut cx) else {
                panic!("completion");
            };
            assert_eq!(completion.id(), id);
            assert!(io.poll_reserve(&mut cx, next).is_pending());
            assert_eq!(completion._key.id(), descriptor.key_id);
            match &completion.outcome {
                CryptoOutcome::Completed(CryptoOutput::Decrypted(clear, original)) => {
                    assert!(!canceled && !corrupt);
                    assert_eq!(clear.bytes(), b"hello");
                    assert_eq!(original.bytes().as_ptr(), original_pointer);
                }
                CryptoOutcome::Failed {
                    input:
                        CryptoInput::Decrypt {
                            ciphertext,
                            plaintext,
                        },
                    error,
                } => {
                    assert_eq!(
                        *error,
                        if canceled {
                            Error::Cancelled
                        } else {
                            Error::CorruptRecord
                        }
                    );
                    assert_eq!(ciphertext.bytes(), bytes);
                    assert_eq!(ciphertext.bytes().as_ptr(), original_pointer);
                    assert_eq!(plaintext.amount(), 5);
                }
                _ => panic!("wrong outcome"),
            }
            assert_eq!(admission.used(ResourceClass::Plaintext), 5);
            assert_eq!(admission.used(ResourceClass::Ciphertext), 21);
            drop(completion);
            assert_eq!(admission.used(ResourceClass::Plaintext), 0);
            assert_eq!(admission.used(ResourceClass::Ciphertext), 0);
        }
    }
    #[test]
    fn encryption_preserves_staging_and_charges_on_failure() {
        use crate::admission::AdmissionPolicy;
        use crate::memory::BufferPool;
        use crate::security::CryptoId;
        use crate::security::pair;
        let keys = crate::test_support::security::keys();
        let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
            crate::test_support::cluster::config(false).limits,
        )));
        let pool = BufferPool::new(admission.clone());
        let page = envelope().page;
        let cache = &page.version.object.cache;
        let (io, port) = pair(WorkerId(0), 1, std::num::NonZeroUsize::new(1).unwrap());
        let mut engine = PageCryptoEngine::new(CryptoRuntime { port });
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        let mut nonces = std::collections::HashSet::new();
        for sequence in 1..=5 {
            let canceled = sequence == 3;
            let undersized = sequence == 4;
            let wrong_cache = sequence == 5;
            let staging_cache = if wrong_cache {
                CacheId(crate::test_support::security::NODE.into())
            } else {
                cache.clone()
            };
            let mut plaintext = pool
                .plaintext(
                    admission
                        .reserve(Some(&staging_cache), ResourceClass::Plaintext, 5)
                        .unwrap(),
                    5,
                )
                .unwrap();
            plaintext.bytes_mut().unwrap().copy_from_slice(b"hello");
            let pointer = plaintext.bytes().unwrap().as_ptr();
            let ciphertext = admission
                .reserve(
                    Some(cache),
                    ResourceClass::Ciphertext,
                    if undersized { 20 } else { 21 },
                )
                .unwrap();
            let scope = RequestScope::new(
                RequestId([1; 16]),
                std::time::Instant::now() + std::time::Duration::from_secs(10),
            )
            .unwrap();
            if canceled {
                scope.cancel().unwrap();
            }
            let id = CryptoId {
                worker: WorkerId(0),
                generation: 1,
                sequence,
            };
            let Poll::Ready(Ok(permit)) = io.poll_reserve(&mut cx, id) else {
                panic!("reserve");
            };
            assert!(
                io.try_submit(permit.job(
                    CryptoInput::Encrypt {
                        page: page.clone(),
                        plaintext,
                        ciphertext
                    },
                    keys.active(cache, KeyPurpose::Page).unwrap(),
                    scope
                ))
                .is_ok()
            );
            engine
                .poll_budgeted(&mut Context::from_waker(futures::task::noop_waker_ref()), 1)
                .unwrap();
            let Poll::Ready(Ok(Some(completion))) = io.poll_completion(&mut cx) else {
                panic!("completion");
            };
            match &completion.outcome {
                CryptoOutcome::Completed(CryptoOutput::Encrypted(clear, cipher)) => {
                    assert!(sequence <= 2);
                    assert_eq!(clear.bytes().as_ptr(), pointer);
                    assert_eq!(clear.bytes(), b"hello");
                    assert!(nonces.insert(cipher.envelope().nonce.0));
                    let mut decrypted = vec![0; cipher.envelope().plaintext_length as usize];
                    racer_crypto::open(
                        &[7; 32],
                        &cipher.envelope().nonce.0,
                        &page_aad(cipher.envelope()).unwrap(),
                        cipher.bytes(),
                        &mut decrypted,
                    )
                    .unwrap();
                    assert_eq!(decrypted, b"hello");
                }
                CryptoOutcome::Failed {
                    input:
                        CryptoInput::Encrypt {
                            plaintext,
                            ciphertext,
                            ..
                        },
                    error,
                } => {
                    assert_eq!(
                        *error,
                        if canceled {
                            Error::Cancelled
                        } else {
                            Error::InvalidConfiguration
                        }
                    );
                    assert_eq!(plaintext.bytes().unwrap().as_ptr(), pointer);
                    assert_eq!(plaintext.bytes().unwrap(), b"hello");
                    assert_eq!(ciphertext.amount(), if undersized { 20 } else { 21 });
                }
                _ => panic!("wrong outcome"),
            }
            assert_eq!(admission.used(ResourceClass::Plaintext), 5);
            drop(completion);
            assert_eq!(admission.used(ResourceClass::Plaintext), 0);
            assert_eq!(admission.used(ResourceClass::Ciphertext), 0);
        }
    }
    #[test]
    fn engine_encrypts_and_returns_original_staging_on_failure() {
        use crate::admission::AdmissionPolicy;
        use crate::memory::BufferPool;
        use crate::security::CryptoId;
        use crate::security::pair;
        let keys = crate::test_support::security::keys();
        let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
            crate::test_support::cluster::config(false).limits,
        )));
        let pool = BufferPool::new(admission.clone());
        let descriptor = envelope();
        let cache = &descriptor.page.version.object.cache;
        let (io, port) = pair(WorkerId(0), 1, std::num::NonZeroUsize::new(1).unwrap());
        let mut engine = PageCryptoEngine::new(CryptoRuntime { port });
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        for (sequence, canceled, undersized) in
            [(1, false, false), (2, true, false), (3, false, true)]
        {
            let mut plaintext = pool
                .plaintext(
                    admission
                        .reserve(Some(cache), ResourceClass::Plaintext, 5)
                        .unwrap(),
                    5,
                )
                .unwrap();
            plaintext.bytes_mut().unwrap().copy_from_slice(b"hello");
            let pointer = plaintext.bytes().unwrap().as_ptr();
            let input = CryptoInput::Encrypt {
                page: descriptor.page.clone(),
                plaintext,
                ciphertext: admission
                    .reserve(
                        Some(cache),
                        ResourceClass::Ciphertext,
                        if undersized { 20 } else { 21 },
                    )
                    .unwrap(),
            };
            let scope = RequestScope::new(
                RequestId([1; 16]),
                std::time::Instant::now() + std::time::Duration::from_secs(10),
            )
            .unwrap();
            if canceled {
                scope.cancel().unwrap();
            }
            let id = CryptoId {
                worker: WorkerId(0),
                generation: 1,
                sequence,
            };
            let Poll::Ready(Ok(permit)) = io.poll_reserve(&mut cx, id) else {
                panic!("reserve")
            };
            assert!(
                io.try_submit(permit.job(
                    input,
                    keys.active(cache, KeyPurpose::Page).unwrap(),
                    scope
                ))
                .is_ok()
            );
            engine
                .poll_budgeted(&mut Context::from_waker(futures::task::noop_waker_ref()), 1)
                .unwrap();
            let Poll::Ready(Ok(Some(completion))) = io.poll_completion(&mut cx) else {
                panic!("completion")
            };
            match &completion.outcome {
                CryptoOutcome::Completed(CryptoOutput::Encrypted(clear, encrypted)) => {
                    assert!(!canceled && !undersized);
                    assert_eq!(clear.bytes(), b"hello");
                    assert_eq!(clear.bytes().as_ptr(), pointer);
                    let mut bytes = vec![0; encrypted.envelope().plaintext_length as usize];
                    racer_crypto::open(
                        &[7; 32],
                        &encrypted.envelope().nonce.0,
                        &page_aad(encrypted.envelope()).unwrap(),
                        encrypted.bytes(),
                        &mut bytes,
                    )
                    .unwrap();
                    assert_eq!(bytes, b"hello");
                }
                CryptoOutcome::Failed {
                    input:
                        CryptoInput::Encrypt {
                            plaintext,
                            ciphertext,
                            ..
                        },
                    error,
                } => {
                    assert_eq!(
                        *error,
                        if canceled {
                            Error::Cancelled
                        } else {
                            Error::InvalidConfiguration
                        }
                    );
                    assert_eq!(plaintext.bytes().unwrap(), b"hello");
                    assert_eq!(plaintext.bytes().unwrap().as_ptr(), pointer);
                    assert_eq!(ciphertext.amount(), if undersized { 20 } else { 21 });
                }
                _ => panic!("wrong outcome"),
            }
            assert!(
                io.poll_reserve(
                    &mut cx,
                    CryptoId {
                        sequence: sequence + 1,
                        ..id
                    }
                )
                .is_pending()
            );
            assert_eq!(admission.used(ResourceClass::Plaintext), 5);
            drop(completion);
            assert_eq!(admission.used(ResourceClass::Plaintext), 0);
            assert_eq!(admission.used(ResourceClass::Ciphertext), 0);
        }
    }
    #[test]
    fn drain_reschedules_after_a_full_quantum() {
        use crate::admission::AdmissionPolicy;
        use crate::security::CryptoId;
        use crate::security::pair;
        use std::sync::atomic::AtomicUsize;
        use std::sync::atomic::Ordering;
        struct WakeCount(AtomicUsize);
        impl std::task::Wake for WakeCount {
            fn wake(self: Arc<Self>) {
                self.0.fetch_add(1, Ordering::SeqCst);
            }
            fn wake_by_ref(self: &Arc<Self>) {
                self.0.fetch_add(1, Ordering::SeqCst);
            }
        }
        let keys = crate::test_support::security::keys();
        let admission = flow_control::Quotas::new(AdmissionPolicy::new(
            crate::test_support::cluster::config(false).limits,
        ));
        let descriptor = envelope();
        let cache = &descriptor.page.version.object.cache;
        let scope = RequestScope::new(
            RequestId([1; 16]),
            std::time::Instant::now() + std::time::Duration::from_secs(10),
        )
        .unwrap();
        let (io, port) = pair(WorkerId(0), 1, std::num::NonZeroUsize::new(9).unwrap());
        let wake = Arc::new(WakeCount(AtomicUsize::new(0)));
        let waker = std::task::Waker::from(wake.clone());
        let mut cx = Context::from_waker(&waker);
        let mut engine = PageCryptoEngine::new(CryptoRuntime { port });
        engine.register_driver(&waker);
        // Budget polling's noop task waker must not replace the runtime driver.
        engine
            .poll_budgeted(&mut Context::from_waker(futures::task::noop_waker_ref()), 1)
            .unwrap();
        for sequence in 1..=9 {
            let job_scope =
                RequestScope::new(RequestId([sequence as u8; 16]), scope.deadline.0).unwrap();
            if sequence % 3 == 2 {
                job_scope.cancel().unwrap();
            }
            let mut bytes = vec![0; 5 + TAG_LEN];
            racer_crypto::seal(
                &[7; 32],
                &descriptor.nonce.0,
                &page_aad(&descriptor).unwrap(),
                b"hello",
                &mut bytes,
            )
            .unwrap();
            if sequence % 3 == 0 {
                bytes[0] ^= 1;
            }
            let Poll::Ready(Ok(permit)) = io.poll_reserve(
                &mut cx,
                CryptoId {
                    worker: WorkerId(0),
                    generation: 1,
                    sequence,
                },
            ) else {
                panic!("reserve");
            };
            let cipher = CiphertextPage {
                provenance: None,
                inner: Arc::new(CiphertextBytes {
                    checksum: std::sync::OnceLock::new(),
                    envelope: descriptor.clone(),
                    bytes,
                    reservation: admission
                        .reserve(Some(cache), ResourceClass::Ciphertext, 21)
                        .unwrap(),
                }),
            };
            let input = CryptoInput::Decrypt {
                ciphertext: cipher,
                plaintext: admission
                    .reserve(Some(cache), ResourceClass::Plaintext, 5)
                    .unwrap(),
            };
            assert!(
                io.try_submit(permit.job(
                    input,
                    keys.active(cache, KeyPurpose::Page).unwrap(),
                    job_scope
                ))
                .is_ok()
            );
        }
        io.close_submissions().unwrap();
        assert!(wake.0.swap(0, Ordering::SeqCst) > 0);
        let mut drain = engine.drain(&scope);
        for sequence in 1..=9 {
            assert!(drain.as_mut().poll(&mut cx).is_pending());
            assert!(wake.0.swap(0, Ordering::SeqCst) > 0);
            let Poll::Ready(Ok(Some(completion))) = io.poll_completion(&mut cx) else {
                panic!("one completion per drain poll");
            };
            assert_eq!(completion.id().sequence, sequence);
            match &completion.outcome {
                CryptoOutcome::Completed(CryptoOutput::Decrypted(clear, _)) => {
                    assert_eq!(sequence % 3, 1);
                    assert_eq!(clear.bytes(), b"hello");
                }
                CryptoOutcome::Failed { error, .. } => {
                    assert_eq!(
                        *error,
                        if sequence % 3 == 2 {
                            Error::Cancelled
                        } else {
                            Error::CorruptRecord
                        }
                    );
                }
                _ => panic!("unexpected drain completion"),
            }
            assert!(io.poll_completion(&mut cx).is_pending());
            drop(completion);
            assert_eq!(
                admission.used(ResourceClass::Plaintext),
                (9 - sequence) as usize * 5
            );
        }
        assert!(matches!(drain.as_mut().poll(&mut cx), Poll::Ready(Ok(()))));
        assert_eq!(admission.used(ResourceClass::Plaintext), 0);
    }
}
