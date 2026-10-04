//! Vetted XChaCha20-Poly1305 adapter, canonical page AAD, fresh cryptographic nonces.

impl From<racer_identity::Error> for crate::error::Error {
    fn from(error: racer_identity::Error) -> Self {
        match error {
            racer_identity::Error::InvalidRequest => Self::InvalidRequest,
            racer_identity::Error::InvalidConfiguration => Self::InvalidConfiguration,
            racer_identity::Error::Unauthorized => Self::Unauthorized,
            racer_identity::Error::Unavailable => Self::Unavailable,
            racer_identity::Error::MissingKey => Self::MissingKey,
            racer_identity::Error::CorruptRecord => Self::CorruptRecord,
        }
    }
}
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
use crate::model::KeyId;
use crate::model::MAX_FIELD_BYTES;
use crate::model::Nonce;
use crate::model::ObjectId;
use crate::model::PageEnvelope;
use crate::model::PageId;
use crate::model::RequestId;
use crate::runtime::RequestScope;
use crate::runtime::worker::CryptoRuntime;
use crate::telemetry::AeadFailure;
use crate::telemetry::Failure;
use crate::telemetry::Stage;
use racer_crypto::aead;
use racer_identity::KeyPurpose;
use racer_identity::Keyring;
use std::fmt;
use std::ops::Deref;
use std::rc::Rc;
use std::sync::Arc;
use std::task::Context;
use std::task::Poll;
use zeroize::Zeroizing;

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
    if !racer_identity::canonical_uuid(&envelope.page.version.object.cache.0)
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
    pending: Option<CryptoCompletion>,
    closed: bool,
}
impl PageCryptoEngine {
    pub fn new(runtime: CryptoRuntime) -> Self {
        Self {
            environment: uring_runtime::environment::Environment::current(),
            runtime,
            pending: None,
            closed: false,
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
        if let Some(sample) = &mut permit.send_sample {
            if let CryptoInput::Checksum { ciphertext } = &input {
                if let Ok(aad) = page_aad(ciphertext.envelope()) {
                    sample.facts = Some(capture_aead_failure(ciphertext, &aad, scope.request));
                }
            }
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
        key: &racer_identity::KeyLease,
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
                    || length.checked_add(aead::TAG_LEN) != Some(ciphertext.bytes().len())
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
        let mut exhausted = budget != 0;
        for _ in 0..budget {
            if let Some(completion) = self.pending.take() {
                if let Err(failure) = self.runtime.port.complete(completion) {
                    self.pending = Some(failure.command);
                    return Err(failure.error);
                }
            }
            match self.runtime.port.poll_job(cx) {
                Poll::Pending => {
                    exhausted = false;
                    break;
                }
                Poll::Ready(Err(error)) => return Err(error),
                Poll::Ready(Ok(None)) => {
                    self.closed = true;
                    exhausted = false;
                    break;
                }
                Poll::Ready(Ok(Some(job))) => self.pending = Some(Self::process(job)),
            }
        }
        // The reserved completion must be published even at the last budget unit.
        if let Some(completion) = self.pending.take() {
            if let Err(failure) = self.runtime.port.complete(completion) {
                self.pending = Some(failure.command);
                return Err(failure.error);
            }
        }
        if exhausted {
            cx.waker().wake_by_ref();
        }
        Ok(())
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
            if self.closed && self.pending.is_none() {
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
    if !racer_identity::canonical_uuid(&object.cache.0)
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
            let mut bytes = Zeroizing::new(vec![0; raw.len() + aead::TAG_LEN]);
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
            let mut bytes = Zeroizing::new(vec![0; encrypted.ciphertext.len() - aead::TAG_LEN]);
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
use crate::model::WorkerId;
use racer_identity::KeyLease;
use std::cell::Cell;
use std::cell::RefCell;
use std::collections::BTreeMap;
use std::collections::VecDeque;
use std::num::NonZeroUsize;
use std::sync::atomic::AtomicBool;
use std::sync::atomic::AtomicUsize;
use std::sync::atomic::Ordering;
use std::task::Waker;
use uring_runtime::channel;
use uring_runtime::channel::Receiver;
use uring_runtime::channel::Sender;

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
/// I/O-generated identity, independent of flights. Never reuse a sequence within
/// a pair generation; restart increments the generation and rejects late results.
#[derive(Clone, Copy, Debug, Eq, Hash, PartialEq)]
pub struct CryptoId {
    pub worker: WorkerId,
    pub generation: u64,
    pub sequence: u64,
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
/// Composition descriptor shared only by the two endpoints and their permits.
/// Bound queued + executing + unconsumed completions by capacity, not merely jobs.
struct Handoff {
    worker: WorkerId,
    generation: u64,
    capacity: NonZeroUsize,
    outstanding: AtomicUsize,
    closed: AtomicBool,
    capacity_waker: futures::task::AtomicWaker,
    engine_waker: futures::task::AtomicWaker,
    io_waker: futures::task::AtomicWaker,
}
/// Non-cloneable, pair-bound reservation of both submission and completion space.
/// Only the I/O endpoint can mint it. Failed submission returns the entire job.
/// ```compile_fail
/// use racer_dataplane::security::CryptoPermit;
/// fn duplicate(permit: CryptoPermit) { let _second = permit.clone(); }
/// ```
pub struct CryptoPermit {
    pub(crate) send_sample: Option<crate::telemetry::Work>,
    handoff: Arc<Handoff>,
    id: CryptoId,
    measurement: Measurement,
    pub(crate) aead_failure: Option<crate::telemetry::AeadFailure>,
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
            let _ = metrics.record(event, amount);
        }
        if let Some(rejection) = m.rejection {
            let _ = metrics.record(
                match rejection {
                    IntegrityRejection::Crc => CryptoDecryptCrcRejected,
                    IntegrityRejection::Aead => CryptoDecryptAeadRejected,
                },
                1,
            );
        }
    }
}
impl Drop for CryptoPermit {
    fn drop(&mut self) {
        self.handoff.outstanding.fetch_sub(1, Ordering::AcqRel);
        self.handoff.capacity_waker.wake();
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
        self.permit.id
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
        self.permit.id
    }
}
/// Endpoints move to their respective threads before constructing local services.
/// They are not Clone: exactly one producer/consumer exists in each direction.
pub struct IoCryptoPort {
    handoff: Arc<Handoff>,
    jobs: Sender<CryptoJob>,
    completions: RefCell<Receiver<CryptoCompletion>>,
    last_sequence: Cell<Option<u64>>,
}
pub struct CryptoPort {
    handoff: Arc<Handoff>,
    jobs: Option<Receiver<CryptoJob>>,
    completions: Sender<CryptoCompletion>,
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
    let (jobs_tx, jobs_rx) = channel::bounded(capacity.get())?;
    let (results_tx, results_rx) = channel::bounded(capacity.get())?;
    let handoff = Arc::new(Handoff {
        worker,
        generation,
        capacity,
        outstanding: AtomicUsize::new(0),
        closed: AtomicBool::new(false),
        capacity_waker: futures::task::AtomicWaker::new(),
        engine_waker: futures::task::AtomicWaker::new(),
        io_waker: futures::task::AtomicWaker::new(),
    });
    Ok((
        IoCryptoPort {
            handoff: handoff.clone(),
            jobs: jobs_tx,
            completions: RefCell::new(results_rx),
            last_sequence: Cell::new(None),
        },
        CryptoPort {
            handoff,
            jobs: Some(jobs_rx),
            completions: results_tx,
        },
    ))
}
impl IoCryptoPort {
    /// Atomically reserve both directions. Saturation parks with a wakeup rather
    /// than holding a job-only slot. Reject wrong-worker/stale IDs.
    pub fn poll_reserve(&self, cx: &mut Context<'_>, id: CryptoId) -> Poll<Result<CryptoPermit>> {
        self.handoff.capacity_waker.register(cx.waker());
        if self.handoff.closed.load(Ordering::Acquire) {
            return Poll::Ready(Err(Error::Unavailable));
        }
        if id.worker != self.handoff.worker
            || id.generation != self.handoff.generation
            || self
                .last_sequence
                .get()
                .is_some_and(|last| id.sequence <= last)
        {
            return Poll::Ready(Err(Error::StaleFlight));
        }
        if self
            .handoff
            .outstanding
            .fetch_update(Ordering::AcqRel, Ordering::Acquire, |n| {
                (n < self.handoff.capacity.get()).then_some(n + 1)
            })
            .is_err()
        {
            return Poll::Pending;
        }
        self.last_sequence.set(Some(id.sequence));
        Poll::Ready(Ok(CryptoPermit {
            send_sample: None,
            aead_failure: None,
            handoff: self.handoff.clone(),
            id,
            measurement: Measurement::default(),
        }))
    }
    /// Rejected submission returns all ownership; accepted jobs outlive waiters.
    pub fn try_submit(
        &self,
        mut job: CryptoJob,
    ) -> std::result::Result<(), CryptoSendFailure<CryptoJob>> {
        if !Arc::ptr_eq(&self.handoff, &job.permit.handoff)
            || self.handoff.closed.load(Ordering::Acquire)
        {
            return Err(CryptoSendFailure {
                command: job,
                error: Error::Unavailable,
            });
        }
        // Capture at publication attempt, not reservation. Retry overwrites it.
        job.permit.measurement.submitted = Some(uring_runtime::environment::now());
        self.jobs.try_send(job)?;
        self.handoff.engine_waker.wake();
        Ok(())
    }
    /// Drain even abandoned/stale completions before returning their credits.
    pub fn poll_completion(&self, cx: &mut Context<'_>) -> Poll<Result<Option<CryptoCompletion>>> {
        self.completions
            .borrow_mut()
            .poll_receive(cx)
            .map_err(Into::into)
    }
    /// Refuse new reservations/submissions, but keep completions available.
    pub fn close_submissions(&self) -> Result<()> {
        self.handoff.closed.store(true, Ordering::Release);
        self.jobs.close();
        self.handoff.engine_waker.wake();
        self.handoff.capacity_waker.wake();
        Ok(())
    }
}
impl CryptoPort {
    /// Worker-level wake registration independent of per-operation polling.
    pub fn register_driver(&self, waker: &Waker) {
        self.handoff.engine_waker.register(waker);
    }
    /// None means closed and all accepted jobs consumed, not temporarily empty.
    pub fn poll_job(&mut self, cx: &mut Context<'_>) -> Poll<Result<Option<CryptoJob>>> {
        let result = self
            .jobs
            .as_mut()
            .expect("live engine endpoint")
            .poll_receive(cx)
            .map_err(Into::into);
        match result {
            Poll::Ready(Ok(Some(mut job))) => {
                job.permit.measurement.queue_ns = job.permit.measurement.submitted.map(elapsed_ns);
                Poll::Ready(Ok(Some(job)))
            }
            other => other,
        }
    }
    /// Reserved completion slots survive drain; failure returns engine ownership.
    pub fn complete(
        &mut self,
        completion: CryptoCompletion,
    ) -> std::result::Result<(), CryptoSendFailure<CryptoCompletion>> {
        if !Arc::ptr_eq(&self.handoff, &completion.permit.handoff) {
            return Err(CryptoSendFailure {
                command: completion,
                error: Error::StaleFlight,
            });
        }
        self.completions.try_send(completion)?;
        self.handoff.io_waker.wake();
        Ok(())
    }
}
impl Drop for CryptoPort {
    fn drop(&mut self) {
        // No engine accesses queued jobs. Drain unique-consumer ownership before EOF.
        self.handoff.closed.store(true, Ordering::Release);
        if let Some(mut jobs) = self.jobs.take() {
            while let Ok(Some(job)) = jobs.receive() {
                drop(job);
            }
        }
        self.completions.close();
        self.handoff.io_waker.wake();
        self.handoff.capacity_waker.wake();
    }
}
/// Local submission facade driven by the worker, not by the waiting future.
/// Future drop abandons delivery only; completion reap fences retained resources.
/// I/O checks generation/sequence before delivery and never publishes stale work.
pub struct CryptoClient {
    observer: RefCell<crate::telemetry::Observer>,
    port: IoCryptoPort,
    metrics: RefCell<Option<crate::telemetry::Metrics>>,
    waiters: RefCell<BTreeMap<CryptoId, Waiter>>,
    sequence: Cell<u64>,
    pending: RefCell<VecDeque<(CryptoId, Waker, RequestScope)>>,
    deadline_cursor: Cell<Option<CryptoId>>,
    pending_cursor: Cell<usize>,
    drain_waiter: RefCell<Option<(RequestScope, Waker)>>,
}
struct Waiter {
    waker: Waker,
    abandoned: bool,
    result: Option<CryptoCompletion>,
}
struct Registration<'a> {
    client: &'a CryptoClient,
    id: CryptoId,
}
struct CapacityWaiter<'a> {
    client: &'a CryptoClient,
    id: CryptoId,
}
impl Drop for CapacityWaiter<'_> {
    fn drop(&mut self) {
        let wake = {
            let mut pending = self.client.pending.borrow_mut();
            pending.retain(|(id, _, _)| *id != self.id);
            pending.front().map(|(_, waker, _)| waker.clone())
        };
        if let Some(waker) = wake {
            waker.wake();
        }
    }
}
impl Drop for Registration<'_> {
    fn drop(&mut self) {
        let mut waiters = self.client.waiters.borrow_mut();
        if let Some(waiter) = waiters.get_mut(&self.id) {
            if waiter.result.is_some() {
                waiters.remove(&self.id);
            } else {
                waiter.abandoned = true;
            }
        }
    }
}
impl CryptoClient {
    pub fn new(port: IoCryptoPort) -> Self {
        Self {
            observer: RefCell::new(crate::telemetry::Observer::default()),
            port,
            metrics: RefCell::new(None),
            waiters: RefCell::new(BTreeMap::new()),
            sequence: Cell::new(0),
            pending: RefCell::new(VecDeque::new()),
            deadline_cursor: Cell::new(None),
            pending_cursor: Cell::new(0),
            drain_waiter: RefCell::new(None),
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
            scope.check()?;
            let cancellation = scope.cancellation.subscribe()?;
            if self.pending.borrow().len() >= self.port.handoff.capacity.get() {
                return Err(Error::Overloaded);
            }
            let sequence = self
                .sequence
                .get()
                .checked_add(1)
                .ok_or(Error::Unavailable)?;
            self.sequence.set(sequence);
            let id = CryptoId {
                worker: self.port.handoff.worker,
                generation: self.port.handoff.generation,
                sequence,
            };
            let capacity_waiter = CapacityWaiter { client: self, id };
            let mut permit = futures::future::poll_fn(|cx| {
                cancellation.register(cx.waker());
                scope.check()?;
                let mut pending = self.pending.borrow_mut();
                if let Some((_, waker, _)) = pending
                    .iter_mut()
                    .find(|(candidate, _, _)| *candidate == id)
                {
                    *waker = cx.waker().clone();
                } else {
                    pending.push_back((id, cx.waker().clone(), scope.clone()));
                }
                if pending
                    .front()
                    .is_some_and(|(candidate, _, _)| *candidate != id)
                {
                    return Poll::Pending;
                }
                drop(pending);
                self.port.poll_reserve(cx, id)
            })
            .await?;
            drop(capacity_waiter);
            if let Some(sample) = &sample {
                sample.identify(id);
            }
            permit.send_sample = sample;
            let mut job = Some(permit.job(input, key, scope.clone()));
            futures::future::poll_fn(|cx| {
                self.waiters.borrow_mut().insert(
                    id,
                    Waiter {
                        waker: cx.waker().clone(),
                        abandoned: false,
                        result: None,
                    },
                );
                Poll::Ready(())
            })
            .await;
            let registration = Registration { client: self, id };
            if let Err(failure) = self.port.try_submit(job.take().unwrap()) {
                self.waiters.borrow_mut().remove(&id);
                return Err(failure.error);
            }
            let result = futures::future::poll_fn(|cx| {
                // Returning after acceptance is itself a completion fence; scope
                // failure cannot elect a replacement acquisition before that fence.
                let mut waiters = self.waiters.borrow_mut();
                let Some(waiter) = waiters.get_mut(&id) else {
                    return Poll::Ready(Err(Error::Cancelled));
                };
                waiter.waker = cx.waker().clone();
                if let Some(completion) = waiter.result.take() {
                    let canceled = waiter.abandoned;
                    waiters.remove(&id);
                    if canceled {
                        return Poll::Ready(Err(Error::Cancelled));
                    }
                    if let Err(error) = scope.check() {
                        return Poll::Ready(Err(error));
                    }
                    Poll::Ready(match completion.outcome {
                        CryptoOutcome::Completed(output) => Ok(output),
                        CryptoOutcome::Failed { error, .. } => Err(error),
                    })
                } else if self.port.completions.borrow().is_closed() && self.outstanding() == 0 {
                    Poll::Ready(Err(Error::Unavailable))
                } else {
                    Poll::Pending
                }
            })
            .await;
            drop(registration);
            result
        })
    }
    /// I/O reaps even when no user futures remain, before admitting more work.
    pub fn poll_budgeted(&self, work_budget: usize) -> Result<()> {
        for _ in 0..work_budget {
            let completion = self.port.completions.borrow_mut().receive()?;
            let Some(completion) = completion else {
                break;
            };
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
            let wake = {
                let mut waiters = self.waiters.borrow_mut();
                if let Some(waiter) = waiters.get_mut(&id) {
                    if waiter.abandoned {
                        waiters.remove(&id).map(|waiter| waiter.waker)
                    } else {
                        let wake = waiter.waker.clone();
                        waiter.result = Some(completion);
                        Some(wake)
                    }
                } else {
                    None
                }
            };
            if let Some(wake) = wake {
                wake.wake();
            }
        }
        if self.port.completions.borrow().is_closed() {
            self.port.jobs.discard_closed();
            if self.outstanding() == 0 {
                let abandoned: Vec<_> = self
                    .waiters
                    .borrow()
                    .iter()
                    .filter(|(_, waiter)| waiter.abandoned)
                    .take(work_budget)
                    .map(|(id, _)| *id)
                    .collect();
                for id in abandoned {
                    self.waiters.borrow_mut().remove(&id);
                }
            }
        }
        // Original deadlines bound every wait. The worker drives expiry even
        // without external I/O so canceled futures can detach.
        let wakes: Vec<_> = {
            use std::ops::Bound::Excluded;
            use std::ops::Bound::Unbounded;
            let waiters = self.waiters.borrow();
            let start = self.deadline_cursor.get().map_or(Unbounded, Excluded);
            let mut wakes = Vec::new();
            for (id, waiter) in waiters
                .range((start, Unbounded))
                .chain(waiters.iter())
                .take(work_budget.min(waiters.len()))
            {
                self.deadline_cursor.set(Some(*id));
                if !waiter.abandoned && self.port.completions.borrow().is_closed() {
                    wakes.push(waiter.waker.clone());
                }
            }
            wakes
        };
        for waker in wakes {
            waker.wake();
        }
        let expired: Vec<_> = {
            let pending = self.pending.borrow();
            let mut wakes = Vec::new();
            for _ in 0..work_budget.min(pending.len()) {
                let index = self.pending_cursor.get() % pending.len();
                self.pending_cursor.set(index + 1);
                let (_, waker, scope) = &pending[index];
                if scope.check().is_err() || self.port.handoff.closed.load(Ordering::Acquire) {
                    wakes.push(waker.clone());
                }
            }
            wakes
        };
        for waker in expired {
            waker.wake();
        }
        let drain_wake = self
            .drain_waiter
            .borrow()
            .as_ref()
            .filter(|(scope, _)| scope.check().is_err())
            .map(|(_, waker)| waker.clone());
        if let Some(waker) = drain_wake {
            waker.wake();
        }
        Ok(())
    }
    pub fn outstanding(&self) -> usize {
        self.port.handoff.outstanding.load(Ordering::Acquire)
    }
    pub fn register_driver(&self, waker: &Waker) {
        self.port.handoff.io_waker.register(waker);
    }
    pub fn close_submissions(&self) -> Result<()> {
        self.port.close_submissions()
    }
    /// Deadline cancels delivery, not the ownership fence. Keep polling until all
    /// accepted jobs complete; a timeout cannot authorize dropping live buffers.
    pub fn drain<'a>(&'a self, scope: &'a RequestScope) -> Operation<'a, ()> {
        Box::pin(async move {
            let cancellation = scope.cancellation.subscribe()?;
            struct DrainGuard<'a>(&'a CryptoClient);
            impl Drop for DrainGuard<'_> {
                fn drop(&mut self) {
                    self.0.drain_waiter.borrow_mut().take();
                }
            }
            let _guard = DrainGuard(self);
            futures::future::poll_fn(move |cx| {
                if scope.check().is_ok() {
                    cancellation.register(cx.waker());
                }
                *self.drain_waiter.borrow_mut() = Some((scope.clone(), cx.waker().clone()));
                self.register_driver(cx.waker());
                self.port.handoff.capacity_waker.register(cx.waker());
                self.poll_budgeted(self.port.handoff.capacity.get())?;
                if scope.check().is_err() {
                    let wakes: Vec<_> = self
                        .waiters
                        .borrow()
                        .values()
                        .filter(|waiter| waiter.result.is_some())
                        .map(|waiter| waiter.waker.clone())
                        .collect();
                    for waiter in self.waiters.borrow_mut().values_mut() {
                        waiter.abandoned = true;
                    }
                    self.waiters
                        .borrow_mut()
                        .retain(|_, waiter| waiter.result.is_none());
                    for waker in wakes {
                        waker.wake();
                    }
                }
                if self.outstanding() == 0 {
                    Poll::Ready(Ok(()))
                } else {
                    Poll::Pending
                }
            })
            .await
        })
    }
}
#[cfg(test)]
mod tests {
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
    mod credentials;
    mod crypto;
    use super::*;
    use crate::model::KeyId;
    use crate::model::*;
    use uring_runtime::group::Service;
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
        let mut bytes = vec![0; 5 + aead::TAG_LEN];
        aead::seal(&[7; 32], &original.nonce.0, &aad, b"hello", &mut bytes).unwrap();
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
                aead::open(
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
        assert!(aead::open(&[7; 32], &original.nonce.0, &aad, &corrupted, &mut output).is_err());
        aead::open(&[7; 32], &original.nonce.0, &aad, &bytes, &mut output).unwrap();
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
        let mut bytes = vec![0; 5 + aead::TAG_LEN];
        aead::seal(
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
            aead::seal(
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
            aead::seal(
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
        let mut encrypted = [0; 5 + aead::TAG_LEN];
        aead::seal(
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
            assert!(aead::open(&[7; 32], &nonce, &aad, &input, &mut output).is_err());
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
            let mut bytes = vec![0; 5 + aead::TAG_LEN];
            aead::seal(
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
                    aead::open(
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
                    aead::open(
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
            let mut bytes = vec![0; 5 + aead::TAG_LEN];
            aead::seal(
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
use uring_runtime::reactor::IoBuffer;
