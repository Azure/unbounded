//! Vetted XChaCha20-Poly1305 adapter, canonical page AAD, fresh cryptographic nonces.
use super::keyring::{KeyPurpose, Keyring};
use crate::{
    error::{Error, Operation, Result},
    memory::pool::{CiphertextBytes, CiphertextPage, PlaintextBuffer, VerifiedBytes, VerifiedPage},
    model::{
        envelope::{Nonce, PageEnvelope},
        identity::PageId,
        limits::ResourceClass,
        range::PAGE_BYTES,
    },
    runtime::{
        admission::Reservation,
        crypto::{
            CryptoClient, CryptoCompletion, CryptoInput, CryptoJob, CryptoOutcome, CryptoOutput,
        },
        deadline::RequestScope,
        reactor::IoBuffer,
        worker::{CryptoRuntime, CryptoService},
    },
};
use chacha20poly1305::{KeyInit, XChaCha20Poly1305, XNonce, aead::AeadInPlace};
use std::{
    rc::Rc,
    sync::Arc,
    task::{Context, Poll},
};
use zeroize::Zeroizing;

pub(crate) fn fresh_nonce() -> Result<Nonce> {
    let mut nonce = [0u8; 24];
    getrandom::getrandom(&mut nonce).map_err(|_| Error::Unavailable)?;
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
    if !super::certificates::canonical_uuid(&envelope.page.version.object.cache.0)
        || envelope.page.version.etag.as_bytes().len() > crate::model::MAX_FIELD_BYTES
        || envelope.plaintext_length == 0
        || u64::from(envelope.plaintext_length) > PAGE_BYTES
        || envelope.ciphertext_length
            != envelope
                .plaintext_length
                .checked_add(16)
                .ok_or(Error::CorruptRecord)?
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
pub struct PageCrypto {
    keys: Rc<Keyring>,
    client: Rc<CryptoClient>,
}
impl PageCrypto {
    /// I/O-local facade. Only owned page inputs and an immutable key lease cross
    /// to the engine; this Rc graph and its futures stay on I/O.
    pub fn new(keys: Rc<Keyring>, client: Rc<CryptoClient>) -> Self {
        Self { keys, client }
    }
    /// Authenticate the entire page before constructing a publishable lease.
    /// Futures stay on I/O, even though inputs/completions are Send:
    /// ```compile_fail
    /// use racer_dataplane::{security::aead::PageCrypto,
    ///     memory::pool::CiphertextPage,
    ///     runtime::{admission::Reservation, deadline::RequestScope}};
    /// fn require_send<T: Send>(_: T) {}
    /// fn move_future(crypto: &PageCrypto, page: CiphertextPage,
    ///     output: Reservation, scope: &RequestScope) {
    ///     require_send(crypto.decrypt(page, output, scope));
    /// }
    /// ```
    pub fn decrypt<'a>(
        &'a self,
        ciphertext: CiphertextPage,
        plaintext: Reservation,
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
                CryptoOutput::Encrypted(..) => Err(Error::CorruptRecord),
            }
        })
    }
    /// Encrypt once; original ciphertext is reused verbatim for peers and disk.
    pub fn encrypt<'a>(
        &'a self,
        page: PageId,
        plaintext: PlaintextBuffer,
        ciphertext: Reservation,
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
                CryptoOutput::Decrypted(..) => Err(Error::CorruptRecord),
            }
        })
    }
}

/// Constructed on the paired crypto thread, without access to the I/O service graph.
/// The vetted AEAD adapter processes owned jobs in bounded quanta, checks the
/// original deadline/cancellation, and returns every accepted job as a completion.
pub struct PageCryptoEngine {
    runtime: CryptoRuntime,
    pending: Option<CryptoCompletion>,
    closed: bool,
}
impl PageCryptoEngine {
    pub fn new(runtime: CryptoRuntime) -> Self {
        Self {
            runtime,
            pending: None,
            closed: false,
        }
    }
    /// All fallible work borrows input. Ownership transfers only after the final
    /// cancellation check, so failures retain the exact input and quota owners.
    pub fn process(job: CryptoJob) -> CryptoCompletion {
        let CryptoJob {
            permit,
            input,
            key,
            scope,
        } = job;
        let prepared: Result<(PageEnvelope, Zeroizing<Vec<u8>>)> = (|| {
            scope.check()?;
            let cipher = XChaCha20Poly1305::new(key.material(KeyPurpose::Page)?.into());
            let (envelope, mut bytes) = match &input {
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
                    if plaintext.reservation().cache() != Some(&page.version.object.cache) {
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
                    if ciphertext.cache() != Some(&page.version.object.cache) {
                        return Err(Error::InvalidConfiguration);
                    }
                    let mut bytes =
                        Zeroizing::new(Vec::with_capacity(envelope.ciphertext_length as usize));
                    bytes.extend_from_slice(raw);
                    cipher
                        .encrypt_in_place(XNonce::from_slice(&envelope.nonce.0), &aad, &mut *bytes)
                        .map_err(|_| Error::CorruptRecord)?;
                    (envelope, bytes)
                }
                CryptoInput::Decrypt {
                    ciphertext,
                    plaintext,
                } => {
                    let envelope = ciphertext.envelope();
                    if key.cache() != &envelope.page.version.object.cache
                        || key.id() != envelope.key_id
                    {
                        return Err(Error::MissingKey);
                    }
                    let aad = page_aad(envelope)?;
                    if ciphertext.bytes().len() != envelope.ciphertext_length as usize {
                        return Err(Error::CorruptRecord);
                    }
                    plaintext
                        .validate(ResourceClass::Plaintext, envelope.plaintext_length as usize)?;
                    if plaintext.cache() != Some(&envelope.page.version.object.cache) {
                        return Err(Error::InvalidConfiguration);
                    }
                    // Detached decryption allocates only plaintext-length storage,
                    // never an uncharged tag-sized tail under plaintext admission.
                    let length = envelope.plaintext_length as usize;
                    let mut bytes = Zeroizing::new(ciphertext.bytes()[..length].to_vec());
                    let tag = chacha20poly1305::Tag::from_slice(&ciphertext.bytes()[length..]);
                    cipher
                        .decrypt_in_place_detached(
                            XNonce::from_slice(&envelope.nonce.0),
                            &aad,
                            &mut bytes,
                            tag,
                        )
                        .map_err(|_| Error::CorruptRecord)?;
                    (envelope.clone(), bytes)
                }
            };
            scope.check()?;
            Ok((envelope, std::mem::take(&mut bytes)))
        })();
        let outcome = match prepared {
            Err(error) => CryptoOutcome::Failed { input, error },
            Ok((envelope, mut bytes)) => match input {
                CryptoInput::Encrypt {
                    page,
                    plaintext,
                    ciphertext,
                } => {
                    let (plain, reservation) = plaintext.into_parts();
                    let plain = VerifiedPage {
                        inner: Arc::new(VerifiedBytes {
                            page,
                            bytes: plain.into_vec(),
                            reservation,
                        }),
                    };
                    let encrypted = CiphertextPage {
                        inner: Arc::new(CiphertextBytes {
                            envelope,
                            bytes: std::mem::take(&mut *bytes),
                            reservation: ciphertext,
                        }),
                    };
                    CryptoOutcome::Completed(CryptoOutput::Encrypted(plain, encrypted))
                }
                CryptoInput::Decrypt {
                    ciphertext,
                    plaintext,
                } => {
                    let page = VerifiedPage {
                        inner: Arc::new(VerifiedBytes {
                            page: envelope.page,
                            bytes: std::mem::take(&mut *bytes),
                            reservation: plaintext,
                        }),
                    };
                    CryptoOutcome::Completed(CryptoOutput::Decrypted(page, ciphertext))
                }
            },
        };
        CryptoCompletion {
            permit,
            outcome,
            key,
            scope,
        }
    }
    fn drive(&mut self, cx: &mut Context<'_>, budget: usize) -> Result<()> {
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
impl CryptoService for PageCryptoEngine {
    fn register_driver(&self, waker: &std::task::Waker) {
        self.runtime.port.register_driver(waker);
    }
    fn start<'a>(&'a mut self, scope: &'a RequestScope) -> Operation<'a, ()> {
        Box::pin(async move { scope.check() })
    }
    fn poll_budgeted(&mut self, work_budget: usize) -> Result<()> {
        self.drive(
            &mut Context::from_waker(futures::task::noop_waker_ref()),
            work_budget,
        )
    }
    fn drain<'a>(&'a mut self, scope: &'a RequestScope) -> Operation<'a, ()> {
        Box::pin(futures::future::poll_fn(move |cx| {
            // Deadline never drops accepted owners; canceled jobs become failures.
            if let Err(error) = self.drive(cx, 8) {
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
#[cfg(test)]
mod tests {
    use super::*;
    use crate::model::{envelope::KeyId, identity::*};
    fn envelope() -> PageEnvelope {
        PageEnvelope {
            page: PageId {
                version: ObjectVersion {
                    object: ObjectId {
                        cache: CacheId(super::super::identity::tests::CACHE.into()),
                        key: CacheKey([3; 32]),
                    },
                    etag: StrongEtag::parse(b"\"v1\"").unwrap(),
                },
                number: PageNumber(7),
            },
            key_id: KeyId([1; 16]),
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
        let cipher = XChaCha20Poly1305::new((&[7; 32]).into());
        let mut bytes = b"hello".to_vec();
        cipher
            .encrypt_in_place(XNonce::from_slice(&original.nonce.0), &aad, &mut bytes)
            .unwrap();
        for index in 0..8 {
            let mut changed = original.clone();
            match index {
                0 => {
                    changed.page.version.object.cache =
                        CacheId(super::super::identity::tests::NODE.into())
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
            let mut tampered = bytes.clone();
            assert!(
                cipher
                    .decrypt_in_place(
                        XNonce::from_slice(&changed.nonce.0),
                        &page_aad(&changed).unwrap(),
                        &mut tampered
                    )
                    .is_err()
            );
        }
        let mut corrupted = bytes.clone();
        corrupted[0] ^= 1;
        assert!(
            cipher
                .decrypt_in_place(XNonce::from_slice(&original.nonce.0), &aad, &mut corrupted)
                .is_err()
        );
        cipher
            .decrypt_in_place(XNonce::from_slice(&original.nonce.0), &aad, &mut bytes)
            .unwrap();
        assert_eq!(bytes, b"hello");
        let mut malformed = original;
        malformed.ciphertext_length += 1;
        assert!(page_aad(&malformed).is_err());
    }
    #[test]
    fn libsodium_independent_known_answer() {
        // Independently generated with libsodium 1.0.18
        // crypto_aead_xchacha20poly1305_ietf_encrypt, not this Rust implementation.
        let descriptor = envelope();
        let mut bytes = b"hello".to_vec();
        XChaCha20Poly1305::new((&[7; 32]).into())
            .encrypt_in_place(
                XNonce::from_slice(&[2; 24]),
                &page_aad(&descriptor).unwrap(),
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
    fn random_nonces_do_not_repeat() {
        let mut seen = std::collections::HashSet::new();
        for _ in 0..1024 {
            assert!(seen.insert(fresh_nonce().unwrap().0));
        }
    }
    #[test]
    fn engine_preserves_failed_inputs_and_completion_capacity() {
        use crate::{
            memory::pool::BufferPool,
            runtime::{
                admission::Admission,
                crypto::{CryptoId, pair},
            },
        };
        let keys = super::super::keyring::tests::keys();
        let admission = Rc::new(Admission::new(
            crate::test_support::cluster::config(false).limits,
        ));
        let pool = BufferPool::new(admission.clone());
        let descriptor = envelope();
        let cache = &descriptor.page.version.object.cache;
        let (io, port) = pair(WorkerId(0), 1, std::num::NonZeroUsize::new(1).unwrap());
        let mut engine = PageCryptoEngine::new(CryptoRuntime { port });
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        for (sequence, canceled, corrupt) in [(1, false, false), (2, true, false), (3, false, true)]
        {
            let mut bytes = b"hello".to_vec();
            XChaCha20Poly1305::new((&[7; 32]).into())
                .encrypt_in_place(
                    XNonce::from_slice(&descriptor.nonce.0),
                    &page_aad(&descriptor).unwrap(),
                    &mut bytes,
                )
                .unwrap();
            if corrupt {
                bytes[0] ^= 1;
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
            engine.poll_budgeted(1).unwrap();
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
            assert_eq!(completion.key.id(), descriptor.key_id);
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
        use crate::{
            memory::pool::BufferPool,
            runtime::{
                admission::Admission,
                crypto::{CryptoId, pair},
            },
        };
        let keys = super::super::keyring::tests::keys();
        let admission = Rc::new(Admission::new(
            crate::test_support::cluster::config(false).limits,
        ));
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
                CacheId(super::super::identity::tests::NODE.into())
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
            engine.poll_budgeted(1).unwrap();
            let Poll::Ready(Ok(Some(completion))) = io.poll_completion(&mut cx) else {
                panic!("completion");
            };
            match &completion.outcome {
                CryptoOutcome::Completed(CryptoOutput::Encrypted(clear, cipher)) => {
                    assert!(sequence <= 2);
                    assert_eq!(clear.bytes().as_ptr(), pointer);
                    assert_eq!(clear.bytes(), b"hello");
                    assert!(nonces.insert(cipher.envelope().nonce.0));
                    let mut decrypted = cipher.bytes().to_vec();
                    XChaCha20Poly1305::new((&[7; 32]).into())
                        .decrypt_in_place(
                            XNonce::from_slice(&cipher.envelope().nonce.0),
                            &page_aad(cipher.envelope()).unwrap(),
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
        use crate::{
            memory::pool::BufferPool,
            runtime::{
                admission::Admission,
                crypto::{CryptoId, pair},
            },
        };
        let keys = super::super::keyring::tests::keys();
        let admission = Rc::new(Admission::new(
            crate::test_support::cluster::config(false).limits,
        ));
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
            engine.poll_budgeted(1).unwrap();
            let Poll::Ready(Ok(Some(completion))) = io.poll_completion(&mut cx) else {
                panic!("completion")
            };
            match &completion.outcome {
                CryptoOutcome::Completed(CryptoOutput::Encrypted(clear, encrypted)) => {
                    assert!(!canceled && !undersized);
                    assert_eq!(clear.bytes(), b"hello");
                    assert_eq!(clear.bytes().as_ptr(), pointer);
                    let mut bytes = encrypted.bytes().to_vec();
                    XChaCha20Poly1305::new((&[7; 32]).into())
                        .decrypt_in_place(
                            XNonce::from_slice(&encrypted.envelope().nonce.0),
                            &page_aad(encrypted.envelope()).unwrap(),
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
        use crate::runtime::{
            admission::Admission,
            crypto::{CryptoId, pair},
        };
        use std::sync::atomic::{AtomicUsize, Ordering};
        struct WakeCount(AtomicUsize);
        impl std::task::Wake for WakeCount {
            fn wake(self: Arc<Self>) {
                self.0.fetch_add(1, Ordering::SeqCst);
            }
            fn wake_by_ref(self: &Arc<Self>) {
                self.0.fetch_add(1, Ordering::SeqCst);
            }
        }
        let keys = super::super::keyring::tests::keys();
        let admission = Admission::new(crate::test_support::cluster::config(false).limits);
        let descriptor = envelope();
        let cache = &descriptor.page.version.object.cache;
        let scope = RequestScope::new(
            RequestId([1; 16]),
            std::time::Instant::now() + std::time::Duration::from_secs(10),
        )
        .unwrap();
        scope.cancel().unwrap();
        let (io, port) = pair(WorkerId(0), 1, std::num::NonZeroUsize::new(9).unwrap());
        let wake = Arc::new(WakeCount(AtomicUsize::new(0)));
        let waker = std::task::Waker::from(wake.clone());
        let mut cx = Context::from_waker(&waker);
        let mut engine = PageCryptoEngine::new(CryptoRuntime { port });
        engine.register_driver(&waker);
        // Budget polling's noop task waker must not replace the runtime driver.
        engine.poll_budgeted(1).unwrap();
        for sequence in 1..=9 {
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
                inner: Arc::new(CiphertextBytes {
                    envelope: descriptor.clone(),
                    bytes: vec![0; 21],
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
                    scope.clone()
                ))
                .is_ok()
            );
        }
        io.close_submissions().unwrap();
        assert!(wake.0.swap(0, Ordering::SeqCst) > 0);
        let mut drain = engine.drain(&scope);
        assert!(drain.as_mut().poll(&mut cx).is_pending());
        assert!(wake.0.load(Ordering::SeqCst) > 0);
        assert!(matches!(drain.as_mut().poll(&mut cx), Poll::Ready(Ok(()))));
        for _ in 0..9 {
            assert!(matches!(
                io.poll_completion(&mut cx),
                Poll::Ready(Ok(Some(_)))
            ));
        }
        assert_eq!(admission.used(ResourceClass::Plaintext), 0);
    }
}
