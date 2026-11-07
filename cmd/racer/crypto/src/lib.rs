//! Cryptographic primitives and Racer certificate identities and key epochs.
//!
//! Primitive callers supply entropy, ensure nonce uniqueness for each key, and
//! choose domain separation. AEAD borrows input and writes directly to caller-owned
//! output without allocating or staging plaintext. Public-key parsing does not
//! establish identity or trust.
//!
//! Callers must protect and erase their input seeds, key buffers, and imported
//! DER. Secret-key ownership and explicit PKCS#8 export retain upstream
//! zeroization. The explicit `cipher/zeroize` and `poly1305/zeroize` dependency
//! features are required in addition to `chacha20poly1305/zeroize`.
//!
//! Public workflows and standard vectors live in `tests/primitives.rs`; private
//! epoch/cache state-space tests remain here. Tests do not establish timing or
//! secret-erasure guarantees. Run `cargo test -p racer-crypto` from the workspace.

// `deny` rather than `forbid` so the single SIMD GHASH module can opt in with a
// scoped `allow`; every other module still rejects `unsafe`.
#![deny(unsafe_code)]
#![deny(missing_docs)]

#[cfg(target_arch = "x86_64")]
mod ghash_x86;

use chacha20poly1305::{
    KeyInit, Tag, XChaCha20Poly1305,
    aead::{AeadInOut, inout::InOutBuf},
};
use ed25519_dalek::{
    Signature, Signer,
    pkcs8::{DecodePrivateKey, EncodePrivateKey},
};
use sha2::{Digest, Sha256};
use zeroize::Zeroizing;

/// Opaque failure of a cryptographic operation or input validation.
/// No application-specific error classification or size policy is imposed.
#[derive(Debug)]
pub struct Error(());

/// Secret signing key. Intentionally does not implement Debug or Clone.
/// The upstream key zeroizes its secret on drop; callers own their seed's lifetime.
pub struct SigningKey(ed25519_dalek::SigningKey);

/// Public verification key. Parsing does not establish trust or identity.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub struct VerifyingKey(ed25519_dalek::VerifyingKey);

/// Number of authentication-tag bytes appended to page or credential ciphertext.
pub const TAG_LEN: usize = 16;

impl SigningKey {
    /// Import a caller-supplied secret seed. No randomness is generated here.
    pub fn from_seed(seed: &[u8; 32]) -> Self {
        Self(ed25519_dalek::SigningKey::from_bytes(seed))
    }

    /// Import an Ed25519 PKCS#8 DER document, rejecting malformed key material.
    pub fn from_pkcs8_der(der: &[u8]) -> Result<Self, Error> {
        ed25519_dalek::SigningKey::from_pkcs8_der(der)
            .map(Self)
            .map_err(|_| Error(()))
    }

    /// Explicit secret export. Both the temporary DER document and returned
    /// bytes are zeroized on drop.
    pub fn to_pkcs8_der(&self) -> Result<Zeroizing<Vec<u8>>, Error> {
        let document = self.0.to_pkcs8_der().map_err(|_| Error(()))?;
        Ok(Zeroizing::new(document.as_bytes().to_vec()))
    }

    /// Return the public verification key corresponding to this signing key.
    pub fn verifying_key(&self) -> VerifyingKey {
        VerifyingKey(self.0.verifying_key())
    }

    /// Sign the exact message bytes without adding framing or domain separation.
    pub fn sign(&self, msg: &[u8]) -> [u8; 64] {
        self.0.sign(msg).to_bytes()
    }
}

impl VerifyingKey {
    /// Parse an encoded public key without establishing trust in its owner.
    pub fn from_bytes(bytes: &[u8; 32]) -> Result<Self, Error> {
        ed25519_dalek::VerifyingKey::from_bytes(bytes)
            .map(Self)
            .map_err(|_| Error(()))
    }

    /// Borrow the public key's 32-byte encoding.
    pub fn as_bytes(&self) -> &[u8; 32] {
        self.0.as_bytes()
    }

    /// Reject malformed signatures and use upstream strict verification,
    /// including its small-order point and scalar malleability checks.
    pub fn verify_strict(&self, msg: &[u8], sig: &[u8]) -> Result<(), Error> {
        let signature = Signature::from_slice(sig).map_err(|_| Error(()))?;
        self.0.verify_strict(msg, &signature).map_err(|_| Error(()))
    }
}

impl std::fmt::Display for Error {
    /// Describe the failure without exposing inputs or cryptographic details.
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("cryptographic operation failed")
    }
}

impl std::error::Error for Error {}

/// Seal into exactly `plaintext.len() + TAG_LEN` bytes as ciphertext followed by tag.
/// The caller must never reuse a nonce with the same key.
/// No allocation or plaintext staging copy is performed.
/// Uses XAES-256-GCM as specified at <https://c2sp.org/XAES-256-GCM>.
/// A malformed output length leaves output untouched; any later error zeroes it.
pub fn seal(
    key: &[u8; 32],
    nonce: &[u8; 24],
    aad: &[u8],
    plaintext: &[u8],
    out: &mut [u8],
) -> Result<(), Error> {
    PageCipher::new(key).seal(nonce, aad, plaintext, out)
}

/// XAES-256-GCM (<https://c2sp.org/XAES-256-GCM>) with the per-key state cached.
///
/// Only the master AES-256 key schedule and the CMAC subkey K1 are retained.
/// Each call derives the per-nonce AES-256-GCM key and runs the bulk AEAD
/// through aws-lc-rs, whose AES-GCM uses AES-NI/VAES and carry-less multiply
/// and opens out of place, so a page decrypts from its source buffer into its
/// destination in one pass.
/// The output is byte-identical to the reference construction.
pub(crate) struct PageCipher {
    // aes is built with its zeroize feature, so the schedule is wiped on drop.
    aes: aes::Aes256,
    k1: Zeroizing<[u8; 16]>,
}

impl PageCipher {
    pub(crate) fn new(key: &[u8; 32]) -> Self {
        use aes::cipher::{BlockCipherEncrypt, KeyInit};
        let aes = aes::Aes256::new(key.into());
        let mut l = Zeroizing::new([0u8; 16]);
        aes.encrypt_block((&mut *l).into());
        // CMAC subkey: K1 = L << 1, reduced by 0x87 when the MSB of L is set.
        let mut k1 = Zeroizing::new([0u8; 16]);
        for i in 0..16 {
            let carry = if i < 15 { l[i + 1] >> 7 } else { 0 };
            k1[i] = (l[i] << 1) | carry;
        }
        k1[15] ^= 0x87 & 0u8.wrapping_sub(l[0] >> 7);
        Self { aes, k1 }
    }

    /// Derive the per-nonce AES-256-GCM key Kx from the first 12 nonce bytes.
    fn derive_key(&self, nonce: &[u8; 24]) -> Result<Zeroizing<[u8; 32]>, Error> {
        use aes::cipher::BlockCipherEncrypt;
        let mut kx = Zeroizing::new([0u8; 32]);
        for (index, half) in kx.chunks_exact_mut(16).enumerate() {
            half[1] = index as u8 + 1;
            half[2] = b'X';
            half[4..].copy_from_slice(&nonce[..12]);
            for (byte, k1) in half.iter_mut().zip(self.k1.iter()) {
                *byte ^= k1;
            }
            self.aes
                .encrypt_block(half.try_into().map_err(|_| Error(()))?);
        }
        Ok(kx)
    }

    /// See [`seal`]. A wrong output length leaves `out` untouched; any later
    /// error zeroes it.
    pub(crate) fn seal(
        &self,
        nonce: &[u8; 24],
        aad: &[u8],
        plaintext: &[u8],
        out: &mut [u8],
    ) -> Result<(), Error> {
        if plaintext.len().checked_add(TAG_LEN) != Some(out.len()) {
            return Err(Error(()));
        }
        let result = (|| {
            let kx = self.derive_key(nonce)?;
            let (ciphertext, tag_out) = out.split_at_mut(plaintext.len());
            // aws-lc-rs seals only in place; sealing is off the receive path.
            ciphertext.copy_from_slice(plaintext);
            let tag = gcm_key(&kx)?
                .seal_in_place_separate_tag(
                    gcm_nonce(nonce),
                    aws_lc_rs::aead::Aad::from(aad),
                    ciphertext,
                )
                .map_err(|_| Error(()))?;
            tag_out.copy_from_slice(tag.as_ref());
            Ok(())
        })();
        if result.is_err() {
            out.fill(0);
        }
        result
    }

    /// See [`open`]. A wrong input or output length leaves `out` untouched;
    /// authentication or any later failure zeroes it.
    pub(crate) fn open(
        &self,
        nonce: &[u8; 24],
        aad: &[u8],
        sealed: &[u8],
        out: &mut [u8],
    ) -> Result<(), Error> {
        let length = sealed.len().checked_sub(TAG_LEN).ok_or(Error(()))?;
        if out.len() != length {
            return Err(Error(()));
        }
        let (ciphertext, tag) = sealed.split_at(length);
        let result = self
            .derive_key(nonce)
            .and_then(|kx| gcm_open(&kx, gcm_nonce(nonce), aad, ciphertext, tag, out));
        if result.is_err() {
            out.fill(0);
        }
        result
    }

    /// Open `inout`, which already holds the ciphertext body, in place against
    /// the separate `tag`. Any failure zeroes `inout`, since a failed tag check
    /// leaves unauthenticated plaintext behind.
    pub(crate) fn open_in_place(
        &self,
        nonce: &[u8; 24],
        aad: &[u8],
        inout: &mut [u8],
        tag: &[u8],
    ) -> Result<(), Error> {
        let result = self
            .derive_key(nonce)
            .and_then(|kx| gcm_open_in_place(&kx, gcm_nonce(nonce), aad, inout, tag));
        if result.is_err() {
            inout.fill(0);
        }
        result
    }
}

/// The AES-GCM nonce: the last 12 bytes of the XAES nonce.
fn gcm_nonce(nonce: &[u8; 24]) -> aws_lc_rs::aead::Nonce {
    aws_lc_rs::aead::Nonce::assume_unique_for_key(std::array::from_fn(|i| nonce[12 + i]))
}

fn gcm_key(key: &[u8; 32]) -> Result<aws_lc_rs::aead::LessSafeKey, Error> {
    let key = aws_lc_rs::aead::UnboundKey::new(&aws_lc_rs::aead::AES_256_GCM, key)
        .map_err(|_| Error(()))?;
    Ok(aws_lc_rs::aead::LessSafeKey::new(key))
}

/// AES-256-GCM open from `ciphertext` straight into `out` in one pass, with no
/// staging copy. The two must not overlap. On error `out` may hold
/// unauthenticated plaintext; callers zero it.
fn gcm_open(
    key: &[u8; 32],
    nonce: aws_lc_rs::aead::Nonce,
    aad: &[u8],
    ciphertext: &[u8],
    tag: &[u8],
    out: &mut [u8],
) -> Result<(), Error> {
    if tag.len() != TAG_LEN || ciphertext.len() != out.len() {
        return Err(Error(()));
    }
    gcm_key(key)?
        .open_separate_gather(nonce, aws_lc_rs::aead::Aad::from(aad), ciphertext, tag, out)
        .map_err(|_| Error(()))
}

/// AES-256-GCM open of `inout` in place. On error `inout` may hold
/// unauthenticated plaintext; callers zero it.
fn gcm_open_in_place(
    key: &[u8; 32],
    nonce: aws_lc_rs::aead::Nonce,
    aad: &[u8],
    inout: &mut [u8],
    tag: &[u8],
) -> Result<(), Error> {
    if tag.len() != TAG_LEN {
        return Err(Error(()));
    }
    gcm_key(key)?
        .open_in_place_separate_tag(nonce, aws_lc_rs::aead::Aad::from(aad), tag, inout)
        .map(|_| ())
        .map_err(|_| Error(()))
}

impl PageCipher {
    /// See [`verify_tag`].
    pub(crate) fn verify_tag(
        &self,
        nonce: &[u8; 24],
        aad: &[&[u8]],
        ciphertext: &[u8],
        tag: &[u8],
    ) -> Result<(), Error> {
        use aes::cipher::{BlockCipherEncrypt, KeyInit};
        if tag.len() != TAG_LEN {
            return Err(Error(()));
        }
        let kx = self.derive_key(nonce)?;
        let aes = aes::Aes256::new((&*kx).into());
        // H = AES_K(0^128) keys GHASH.
        let mut h = Zeroizing::new([0u8; 16]);
        aes.encrypt_block((&mut *h).into());
        let mut ghash = PageGhash::new(&h);
        // Stream AAD fields through a 16-byte carry so split fields hash
        // exactly like their concatenation without staging them.
        let mut carry = [0u8; 16];
        let mut used = 0usize;
        let mut aad_len = 0u64;
        for part in aad {
            aad_len += part.len() as u64;
            let mut part = *part;
            if used > 0 {
                let take = part.len().min(16 - used);
                carry[used..used + take].copy_from_slice(&part[..take]);
                used += take;
                part = &part[take..];
                if used < 16 {
                    continue;
                }
                ghash.update_padded(&carry);
            }
            let whole = part.len() & !15;
            ghash.update_padded(&part[..whole]);
            let tail = &part[whole..];
            carry[..tail.len()].copy_from_slice(tail);
            used = tail.len();
        }
        if used > 0 {
            ghash.update_padded(&carry[..used]);
        }
        ghash.update_padded(ciphertext);
        let mut lengths = [0u8; 16];
        lengths[..8].copy_from_slice(&(aad_len * 8).to_be_bytes());
        lengths[8..].copy_from_slice(&((ciphertext.len() as u64) * 8).to_be_bytes());
        ghash.update_padded(&lengths);
        let s = ghash.finalize();
        // J0 = IV || 0^31 || 1 for a 96-bit IV.
        let mut j0 = Zeroizing::new([0u8; 16]);
        j0[..12].copy_from_slice(&nonce[12..]);
        j0[15] = 1;
        aes.encrypt_block((&mut *j0).into());
        let mut expected = Zeroizing::new([0u8; 16]);
        for i in 0..16 {
            expected[i] = s[i] ^ j0[i];
        }
        if ct_eq(&expected[..], tag) {
            Ok(())
        } else {
            Err(Error(()))
        }
    }
}

/// GHASH with an aggregated PCLMULQDQ/VPCLMULQDQ path on x86_64 and the
/// portable `ghash` crate everywhere else (or when the CPU lacks CLMUL).
enum PageGhash {
    #[cfg(target_arch = "x86_64")]
    Fast(ghash_x86::Ghash),
    Portable(ghash::GHash),
}

impl PageGhash {
    fn new(h: &[u8; 16]) -> Self {
        #[cfg(target_arch = "x86_64")]
        if let Some(fast) = ghash_x86::Ghash::new(h) {
            return Self::Fast(fast);
        }
        Self::Portable(ghash::GHash::new(h.into()))
    }

    fn update_padded(&mut self, data: &[u8]) {
        use ghash::universal_hash::UniversalHash;
        match self {
            #[cfg(target_arch = "x86_64")]
            Self::Fast(g) => g.update_padded(data),
            Self::Portable(g) => g.update_padded(data),
        }
    }

    fn finalize(self) -> [u8; 16] {
        use ghash::universal_hash::UniversalHash;
        match self {
            #[cfg(target_arch = "x86_64")]
            Self::Fast(g) => g.finalize(),
            Self::Portable(g) => g.finalize().into(),
        }
    }
}

/// Check an XAES-256-GCM tag over `aad` (the concatenation of its parts) and
/// `ciphertext` without decrypting. Accepts exactly the inputs [`open`]
/// accepts, at the cost of one GHASH pass plus three AES blocks. Comparison is
/// constant time. Nothing is allocated.
pub fn verify_tag(
    key: &[u8; 32],
    nonce: &[u8; 24],
    aad: &[&[u8]],
    ciphertext: &[u8],
    tag: &[u8],
) -> Result<(), Error> {
    PageCipher::new(key).verify_tag(nonce, aad, ciphertext, tag)
}

fn seal_with<C>(
    cipher: &C,
    nonce: &[u8; 24],
    aad: &[u8],
    plaintext: &[u8],
    out: &mut [u8],
) -> Result<(), Error>
where
    C: AeadInOut<NonceSize = cipher::consts::U24, TagSize = cipher::consts::U16>,
{
    if plaintext.len().checked_add(TAG_LEN) != Some(out.len()) {
        return Err(Error(()));
    }
    let (ciphertext, tag_out) = out.split_at_mut(plaintext.len());
    let buffer = InOutBuf::new(plaintext, ciphertext).map_err(|_| Error(()))?;
    let tag = cipher
        .encrypt_inout_detached(nonce.into(), aad, buffer)
        .map_err(|_| Error(()))?;
    tag_out.copy_from_slice(&tag);
    Ok(())
}

/// Open ciphertext followed by tag into exactly `sealed.len() - TAG_LEN` bytes.
/// Malformed input/output lengths return an error with output untouched.
/// Every other error, including authentication failure, zeroes the whole output:
/// the ciphertext is decrypted in place in `out` while the tag is checked, so
/// callers must not rely on prior contents of `out` surviving a failed open.
/// No allocation or separate staging buffer is used.
pub fn open(
    key: &[u8; 32],
    nonce: &[u8; 24],
    aad: &[u8],
    sealed: &[u8],
    out: &mut [u8],
) -> Result<(), Error> {
    PageCipher::new(key).open(nonce, aad, sealed, out)
}

fn open_with<C>(
    cipher: &C,
    nonce: &[u8; 24],
    aad: &[u8],
    sealed: &[u8],
    out: &mut [u8],
) -> Result<(), Error>
where
    C: AeadInOut<NonceSize = cipher::consts::U24, TagSize = cipher::consts::U16>,
{
    let length = sealed.len().checked_sub(TAG_LEN).ok_or(Error(()))?;
    if out.len() != length {
        return Err(Error(()));
    }
    let (ciphertext, tag) = sealed.split_at(length);
    let tag = Tag::try_from(tag).map_err(|_| Error(()))?;
    let buffer = InOutBuf::new(ciphertext, out).map_err(|_| Error(()))?;
    // Upstream detached decryption validates length and authenticates the input
    // before applying the keystream to output. Keep that ordering on upgrades.
    cipher
        .decrypt_inout_detached(nonce.into(), aad, buffer, &tag)
        .map_err(|_| Error(()))
}

/// The AES-256-GCM key and 96-bit nonce that XAES-256-GCM uses for one
/// 24-byte nonce. Lets a page be decrypted by a plain AES-256-GCM
/// implementation, such as the GPU kernels. The key is wiped on drop.
/// Intentionally does not implement Debug or Clone.
pub struct DerivedPageKey {
    key: Zeroizing<[u8; 32]>,

    nonce: [u8; 12],
}

impl DerivedPageKey {
    /// The derived AES-256 key. Callers must not persist it.
    pub fn key(&self) -> &[u8; 32] {
        &self.key
    }

    /// The 12-byte AES-GCM nonce: the last 12 bytes of the XAES nonce.
    pub fn nonce(&self) -> &[u8; 12] {
        &self.nonce
    }
}

impl DerivedPageKey {
    /// Authenticate and decrypt `inout`, which holds a page's ciphertext body,
    /// in place against the separate 16-byte `tag`, with the AES-256-GCM key
    /// and nonce [`xaes_derive`] produced. Equivalent to [`open`] with the
    /// original key and nonce, without the copy [`open`] makes. Any failure,
    /// including a bad tag length, zeroes `inout`, since a failed tag check
    /// leaves unauthenticated plaintext behind.
    pub fn open_in_place(&self, aad: &[u8], inout: &mut [u8], tag: &[u8]) -> Result<(), Error> {
        let result = gcm_open_in_place(&self.key, self.gcm_nonce(), aad, inout, tag);
        if result.is_err() {
            inout.fill(0);
        }
        result
    }

    /// Authenticate and decrypt the ciphertext body `ciphertext` against the
    /// separate 16-byte `tag`, writing the plaintext to `out` in one pass.
    /// `out` must be exactly as long as `ciphertext` and must not overlap it.
    /// Any failure, including a bad length, zeroes `out`, since a failed tag
    /// check leaves unauthenticated plaintext behind.
    pub fn open_into(
        &self,
        aad: &[u8],
        ciphertext: &[u8],
        tag: &[u8],
        out: &mut [u8],
    ) -> Result<(), Error> {
        let result = gcm_open(&self.key, self.gcm_nonce(), aad, ciphertext, tag, out);
        if result.is_err() {
            out.fill(0);
        }
        result
    }

    fn gcm_nonce(&self) -> aws_lc_rs::aead::Nonce {
        aws_lc_rs::aead::Nonce::assume_unique_for_key(self.nonce)
    }
}

/// Derive the AES-256-GCM key and nonce XAES-256-GCM uses for `nonce`, as
/// specified at <https://c2sp.org/XAES-256-GCM>:
/// `L = AES-256(K, 0^128)`, `K1 = L << 1` (xor `0x87` on carry), and the key is
/// `AES-256(K, [0,1,'X',0] || N[..12] ^ K1) || AES-256(K, [0,2,'X',0] || N[..12] ^ K1)`.
/// Shares the derivation [`seal`] and [`open`] use, so the result matches them
/// and `xaes_256_gcm::Xaes256Gcm` 0.1 byte for byte (see the tests).
pub fn xaes_derive(key: &[u8; 32], nonce: &[u8; 24]) -> DerivedPageKey {
    let derived = PageCipher::new(key)
        .derive_key(nonce)
        .expect("16-byte halves always convert to AES blocks");
    let gcm_nonce: [u8; 12] = std::array::from_fn(|i| nonce[12 + i]);
    DerivedPageKey {
        key: derived,
        nonce: gcm_nonce,
    }
}

/// Seal the unchanged XChaCha20-Poly1305 credential format, not the page format.
/// The caller must never reuse a nonce with the same key.
pub fn seal_credentials(
    key: &[u8; 32],
    nonce: &[u8; 24],
    aad: &[u8],
    plaintext: &[u8],
    out: &mut [u8],
) -> Result<(), Error> {
    seal_with(
        &XChaCha20Poly1305::new(key.into()),
        nonce,
        aad,
        plaintext,
        out,
    )
}

/// Open the credential format, leaving output untouched on every error.
pub fn open_credentials(
    key: &[u8; 32],
    nonce: &[u8; 24],
    aad: &[u8],
    sealed: &[u8],
    out: &mut [u8],
) -> Result<(), Error> {
    open_with(&XChaCha20Poly1305::new(key.into()), nonce, aad, sealed, out)
}

/// HMAC-SHA256 with a fixed 32-byte key. Domain separation belongs to the caller.
pub fn hmac_sha256(key: &[u8; 32], msg: &[u8]) -> [u8; 32] {
    let mut inner = Zeroizing::new([0x36; 64]);
    let mut outer = Zeroizing::new([0x5c; 64]);
    for i in 0..32 {
        inner[i] ^= key[i];
        outer[i] ^= key[i];
    }
    let mut hash = Sha256::new();
    hash.update(inner.as_slice());
    hash.update(msg);
    let digest = hash.finalize();
    let mut hash = Sha256::new();
    hash.update(outer.as_slice());
    hash.update(digest);
    hash.finalize().into()
}

/// Compare equal-length slices without data-dependent early exits.
/// Lengths are not secret: unequal lengths return immediately.
pub fn ct_eq(a: &[u8], b: &[u8]) -> bool {
    if a.len() != b.len() {
        return false;
    }
    a.iter().zip(b).fold(0u8, |diff, (a, b)| diff | (a ^ b)) == 0
}

pub mod identity {
    //! Trust roots, certificate validation, and locally generated Ed25519 identities.
    //!
    //! Immutable key leases outlive admission; their last completion owner zeroizes
    //! the secret. Secret export is explicit and zeroizing.
    //!
    //! # Ownership and admission
    //!
    //! Wire bundles move into zeroizing staging before validation. Installation
    //! publishes a complete epoch atomically; removed epochs remain usable only by
    //! previously admitted owners. Leases expose fixed-purpose page/credential AEAD
    //! and request MAC operations, never raw secrets. Inputs are borrowed and outputs
    //! caller-owned. Purpose, cache, and key ID checks do not consult current admission.
    //!
    //! Callers own nonce uniqueness, canonical AAD/messages, quotas, cancellation,
    //! integrity checks, telemetry, and completion ownership. PKCS8 persistence export is
    //! explicit and zeroizing. Errors contain no input data. Certificate caches are
    //! worker-local and non-Send; verified peers cannot be constructed by callers.
    //! This module has no dependency on the dataplane worker or service graph.
    //!
    //! # Validation and fixtures
    //!
    //! Private certificate-cache and epoch state-space tests live here. Public identity
    //! workflows live in `tests/primitives.rs`. Cross-component application
    //! integration tests are outside this extracted workspace.
    //! Run these gates from `cmd/racer`:
    //!
    //! ```sh
    //! timeout --signal=TERM --kill-after=10s 300s cargo test --locked -p racer-crypto
    //! timeout --signal=TERM --kill-after=10s 300s cargo test --locked -p racer-crypto --features test-util
    //! timeout --signal=TERM --kill-after=10s 300s cargo clippy --locked -p racer-crypto --all-targets --all-features --no-deps -- -D warnings
    //! ```
    //!
    //! The opt-in `test-util` feature exposes Ed25519 CA and node-certificate fixtures
    //! with customizable parameters. `test_util::issue_pending` retains an existing
    //! key without consuming additional entropy. Production does not enable it.
    use crate::{SigningKey, VerifyingKey};
    #[cfg(test)]
    use racer_wire::control::CacheKeyPurpose;

    /// Check the canonical UUID spelling used by Racer identity bindings.
    pub use racer_wire::control::valid_uuid as canonical_uuid;
    use racer_wire::control::{
        BundleGeneration, CacheId, CacheKeyRef, CacheKeyState, ClusterId, KEYRING_SCHEMA_VERSION,
        KeyId, MAX_BUNDLE_KEYS, MAX_RETAINED_PAGE_KEYS, NodeId,
    };
    use rustls::{
        RootCertStore,
        pki_types::{CertificateDer, PrivatePkcs8KeyDer},
    };
    use sha2::{Digest, Sha256};
    use std::collections::{HashMap, HashSet};
    use std::{
        cell::{Cell, RefCell},
        collections::VecDeque,
        rc::Rc,
        sync::{
            Arc, Mutex, Weak,
            atomic::{AtomicU64, Ordering},
        },
    };
    use x509_parser::{extensions::GeneralName, parse_x509_certificate};
    use zeroize::Zeroizing;

    /// Payload-free component failures, explicitly mapped by the application.
    #[derive(Clone, Copy, Debug, Eq, PartialEq)]
    pub enum Error {
        /// The requested operation has invalid input.
        InvalidRequest,

        /// The proposed key configuration or epoch transition is invalid.
        InvalidConfiguration,

        /// Identity, trust, or authentication validation failed.
        Unauthorized,

        /// A required resource or coherent publication is temporarily unavailable.
        Unavailable,

        /// No admitted key matches the requested cache, ID, and purpose.
        MissingKey,

        /// A page record or its cryptographic encoding is invalid.
        CorruptRecord,
    }

    /// A component result that never includes secrets in its error payload.
    pub type Result<T> = std::result::Result<T, Error>;

    /// A locally owned Ed25519 key awaiting an authenticated certificate chain.
    pub struct PendingIdentity {
        key: SigningKey,
    }

    /// An accepted node certificate paired with its locally held signing key.
    pub struct SigningIdentity {
        cluster: ClusterId,

        node: NodeId,

        key: SigningKey,

        chain: Vec<Vec<u8>>,

        expires: u64,

        valid_from: u64,

        clock: WallClock,
    }

    /// Per-node wall clock injected at construction, so identity checks need no
    /// ambient runtime state. Host builds read `SystemTime::now`; simulations pass
    /// their virtual clock.
    #[derive(Clone)]
    pub struct WallClock(Arc<dyn Fn() -> std::time::SystemTime + Send + Sync>);

    impl WallClock {
        /// Read the host wall clock.
        pub fn host() -> Self {
            Self(Arc::new(std::time::SystemTime::now))
        }

        /// Read wall time from a caller-owned source, such as a simulated clock.
        pub fn new(now: impl Fn() -> std::time::SystemTime + Send + Sync + 'static) -> Self {
            Self(Arc::new(now))
        }

        /// Current wall time.
        pub fn now(&self) -> std::time::SystemTime {
            (self.0)()
        }

        /// Wall time for rustls, clamped to the Unix epoch for pre-epoch clocks.
        /// This retains rustls's whole-second truncation and does not set certificate policy.
        pub fn unix_time(&self) -> rustls::pki_types::UnixTime {
            rustls::pki_types::UnixTime::since_unix_epoch(
                self.now()
                    .duration_since(std::time::SystemTime::UNIX_EPOCH)
                    .unwrap_or_default(),
            )
        }
    }

    impl std::fmt::Debug for WallClock {
        fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
            f.write_str("WallClock")
        }
    }

    /// Shared publication lock for complete epochs and their signing identity.
    pub struct KeyEpochs {
        state: Mutex<State>,

        /// Advanced with Release after every replacement of the published key
        /// table, so keyring views reload their worker-local index lazily.
        epoch: AtomicU64,

        clock: WallClock,
    }

    impl Default for KeyEpochs {
        /// Epochs checked against the host wall clock.
        fn default() -> Self {
            Self::with_clock(WallClock::host())
        }
    }

    impl KeyEpochs {
        /// Epochs whose identity and trust checks read the given wall clock.
        pub fn with_clock(clock: WallClock) -> Self {
            Self {
                state: Mutex::default(),
                epoch: AtomicU64::new(0),
                clock,
            }
        }

        /// The wall clock used for identity and trust checks.
        pub fn clock(&self) -> &WallClock {
            &self.clock
        }
    }

    /// A cluster/node view of shared epochs, responsible for admission and rotation.
    ///
    /// Each view keeps a local copy of the admission index, reloaded only when
    /// the shared epoch moves, so admission checks and leases take no lock.
    /// The index holds no key material; leases upgrade a weak entry reference.
    #[derive(Clone)]
    pub struct Keyring {
        cluster: ClusterId,

        node: NodeId,

        epochs: Arc<KeyEpochs>,

        /// Epoch of `local`; `u64::MAX` before the first load.
        local_epoch: Cell<u64>,

        local: RefCell<Option<Arc<KeyIndex>>>,
    }

    /// Canonical bundle replay tracking paired with its current keyring owner.
    pub struct BundleInstaller {
        state: RefCell<BundleDelivery>,
    }

    /// Bundle delivery errors preserve replay, codec, and keyring distinctions.
    #[derive(Clone, Copy, Debug, Eq, PartialEq)]
    pub enum BundleError {
        /// A rollback or conflicting reuse of a generation was rejected.
        Replay,

        /// Canonical wire validation failed.
        Wire(racer_wire::control::Error),

        /// The keyring rejected the proposed epoch.
        Identity(Error),
    }

    /// Owner and replay cursor form one worker-local delivery state.
    struct BundleDelivery {
        keys: Rc<Keyring>,

        accepted: Option<BundleGeneration>,
    }

    impl BundleInstaller {
        /// Start delivery tracking for one keyring.
        pub fn new(keys: Rc<Keyring>) -> Self {
            Self {
                state: RefCell::new(BundleDelivery {
                    keys,
                    accepted: None,
                }),
            }
        }

        /// Return the last successfully accepted bundle generation.
        pub fn generation(&self) -> Option<BundleGeneration> {
            self.state.borrow().accepted
        }

        /// Replace the keyring and reset delivery tracking for its new owner.
        pub fn bind_keyring(&self, keys: Rc<Keyring>) {
            *self.state.borrow_mut() = BundleDelivery {
                keys,
                accepted: None,
            };
        }

        /// Canonicalize unordered records, reject replay, and atomically install keys.
        pub fn install(
            &self,
            mut bundle: racer_wire::control::KeyringBundle,
        ) -> std::result::Result<(BundleGeneration, Vec<Vec<u8>>), BundleError> {
            bundle.peer_trust_roots.sort();
            bundle
                .cache_keys
                .sort_by(|a, b| reference_order(&a.key).cmp(&reference_order(&b.key)));
            if bundle.cache_keys.len() > MAX_BUNDLE_KEYS {
                return Err(BundleError::Identity(Error::InvalidConfiguration));
            }
            let mut state = self.state.borrow_mut();
            // Replay is judged once, against the shared published epoch, by
            // `accept_bundle`; exact redelivery is acknowledged without change.
            let roots = bundle.peer_trust_roots.clone();
            let generation = state.keys.install_checked(&bundle.into())?;
            state.accepted = Some(generation);
            Ok((generation, roots))
        }
    }

    /// An immutable secret owner, not a raw-key export capability.
    /// ```compile_fail
    /// fn raw(key: &racer_crypto::identity::KeyLease) {
    ///     key.material(racer_crypto::identity::KeyPurpose::Page);
    /// }
    /// ```
    /// ```compile_fail
    /// fn diagnostic(key: racer_crypto::identity::KeyLease) { println!("{key:?}"); }
    /// ```
    pub struct KeyLease {
        /// The admitted epoch entry; cloning it never allocates.
        entry: Arc<Entry>,

        /// Whether the key was active when leased. Only active keys seal.
        sealable: bool,
    }

    /// The wire-defined purpose permanently bound to a key epoch.
    pub use racer_wire::control::CacheKeyPurpose as KeyPurpose;

    /// Worker-local verifier caching exact chains against the current trust roots.
    /// ```compile_fail
    /// fn send<T: Send>() {}
    /// send::<racer_crypto::identity::Certificates>();
    /// ```
    pub struct Certificates {
        cluster: ClusterId,

        keys: Rc<Keyring>,

        cache: Arc<Mutex<VecDeque<CachedPeer>>>,
    }

    /// Sendable verifier state. Current epochs remain shared, not snapshotted.
    pub struct CertificateVerifier {
        cluster: ClusterId,
        keys: Keyring,
        cache: Arc<Mutex<VecDeque<CachedPeer>>>,
    }

    impl CertificateVerifier {
        /// Rebuild local ownership on the destination thread.
        pub fn into_local(self) -> Certificates {
            Certificates {
                cluster: self.cluster,
                keys: Rc::new(self.keys),
                cache: self.cache,
            }
        }
    }

    /// Only successful certificate validation can construct a verified peer.
    /// ```compile_fail
    /// let peer = racer_crypto::identity::VerifiedPeer { node: racer_wire::control::NodeId("forged".into()) };
    /// ```
    pub struct VerifiedPeer {
        node: NodeId,
    }

    /// An authenticated leaf key and the conservative lifetime of its trust snapshot.
    pub struct ValidatedChain {
        /// The validated leaf public key.
        pub key: VerifyingKey,

        valid_from: u64,

        expires: u64,
    }

    /// Private validation staging owns zeroizing transfers on every rejection exit.
    #[cfg_attr(test, derive(Clone))]
    struct CacheEncryptionKey {
        key: CacheKeyRef,

        state: CacheKeyState,

        material: Zeroizing<[u8; 32]>,
    }

    /// An owned wire transfer with secrets that wipe on every exit path.
    #[cfg_attr(test, derive(Clone))]
    struct KeyringBundle {
        schema_version: u32,

        cluster: ClusterId,

        generation: BundleGeneration,

        peer_trust_roots: Vec<Vec<u8>>,

        cache_keys: Vec<CacheEncryptionKey>,
    }

    impl From<racer_wire::control::KeyringBundle> for KeyringBundle {
        /// Move transferred material into private, zeroizing validation staging.
        fn from(bundle: racer_wire::control::KeyringBundle) -> Self {
            Self {
                schema_version: bundle.schema_version,
                cluster: bundle.cluster,
                generation: bundle.generation,
                peer_trust_roots: bundle.peer_trust_roots,
                cache_keys: bundle
                    .cache_keys
                    .into_iter()
                    .map(|record| {
                        let (key, state, material) = record.into_installation();
                        CacheEncryptionKey {
                            key,
                            state,
                            material,
                        }
                    })
                    .collect(),
            }
        }
    }

    impl PendingIdentity {
        /// Generate a key from operating-system entropy.
        pub fn generate() -> Result<Self> {
            let mut seed = Zeroizing::new([0u8; 32]);
            getrandom::getrandom(&mut *seed).map_err(|_| Error::Unavailable)?;
            Ok(Self::from_seed(&seed))
        }

        /// Derive a key from caller-supplied entropy, such as a seeded simulation source.
        pub fn from_seed(seed: &[u8; 32]) -> Self {
            Self {
                key: SigningKey::from_seed(seed),
            }
        }

        /// Recover a bounded Ed25519 PKCS8 key without accepting a certificate.
        pub fn recover(pkcs8: &[u8]) -> Result<Self> {
            if pkcs8.len() > 4096 {
                return Err(Error::InvalidRequest);
            }
            Ok(Self {
                key: SigningKey::from_pkcs8_der(pkcs8).map_err(|_| Error::Unauthorized)?,
            })
        }

        /// Explicitly export a zeroizing PKCS8 copy for durable storage.
        pub fn export_pkcs8_for_persistence(&self) -> Result<Zeroizing<Vec<u8>>> {
            self.key.to_pkcs8_der().map_err(|_| Error::Unavailable)
        }

        /// The control server assigns the node SAN from authenticated enrollment.
        pub fn csr_der(&self) -> Result<Vec<u8>> {
            let bytes = self.export_pkcs8_for_persistence()?;
            let der = PrivatePkcs8KeyDer::from(bytes.as_slice());
            let key = rcgen::KeyPair::from_pkcs8_der_and_sign_algo(&der, &rcgen::PKCS_ED25519)
                .map_err(|_| Error::Unavailable)?;
            let params = rcgen::CertificateParams::new(Vec::<String>::new())
                .map_err(|_| Error::Unavailable)?;
            Ok(params
                .serialize_request(&key)
                .map_err(|_| Error::Unavailable)?
                .der()
                .to_vec())
        }

        /// Accept only a trusted node chain whose leaf matches this private key.
        pub fn accept(
            self,
            cluster: ClusterId,
            node: NodeId,
            chain: Vec<Vec<u8>>,
            roots: &[Vec<u8>],
            clock: &WallClock,
        ) -> Result<SigningIdentity> {
            let verified = verify_chain(clock, roots, &chain, &cluster, &node)?;
            if verified.key != self.key.verifying_key() {
                return Err(Error::Unauthorized);
            }
            Ok(SigningIdentity {
                cluster,
                node,
                key: self.key,
                expires: verified.expires,
                valid_from: verified.valid_from,
                chain,
                clock: clock.clone(),
            })
        }
    }

    impl SigningIdentity {
        /// Return the conservative expiry of the accepted chain and all roots.
        pub fn expires_at_seconds(&self) -> u64 {
            self.expires
        }

        /// Recover a persisted key and validate its certificate and node binding.
        pub fn from_pkcs8(
            cluster: ClusterId,
            node: NodeId,
            pkcs8: &[u8],
            chain: Vec<Vec<u8>>,
            roots: &[Vec<u8>],
            clock: &WallClock,
        ) -> Result<Self> {
            PendingIdentity::recover(pkcs8)?.accept(cluster, node, chain, roots, clock)
        }

        /// Return the cluster authenticated by the certificate.
        pub fn cluster(&self) -> &ClusterId {
            &self.cluster
        }

        /// Return the node authenticated by the certificate.
        pub fn node(&self) -> &NodeId {
            &self.node
        }

        /// Borrow the exact accepted certificate chain, leaf first.
        pub fn certificate_chain(&self) -> &[Vec<u8>] {
            &self.chain
        }

        /// Sign the exact message only within the accepted validity interval.
        pub fn sign(&self, message: &[u8]) -> Result<Vec<u8>> {
            let now = self.clock.unix_time().as_secs();
            if now < self.valid_from || now >= self.expires {
                return Err(Error::Unauthorized);
            }
            Ok(self.key.sign(message).to_vec())
        }
    }

    /// One chain, keyed by its collision-resistant fingerprint, validated against
    /// a specific immutable root snapshot.
    struct CachedPeer {
        roots: Arc<Vec<Vec<u8>>>,

        fingerprint: [u8; 32],

        node: NodeId,

        key: VerifyingKey,

        until: u64,

        checked: u64,
    }

    /// Length-prefixed digest of an exact chain for the validation cache.
    fn chain_fingerprint(chain: &[Vec<u8>]) -> [u8; 32] {
        let mut hash = Sha256::new();
        hash.update(b"racer-crypto/certificate-chain\0");
        for cert in chain {
            hash.update((cert.len() as u64).to_be_bytes());
            hash.update(cert);
        }
        hash.finalize().into()
    }

    impl VerifiedPeer {
        /// Return the authenticated peer node.
        pub fn node(&self) -> &NodeId {
            &self.node
        }
    }

    impl Certificates {
        /// Create a worker-local verifier using current keyring trust roots.
        pub fn new(cluster: ClusterId, keys: Rc<Keyring>) -> Self {
            Self {
                cluster,
                keys,
                cache: Arc::new(Mutex::new(VecDeque::new())),
            }
        }

        /// Share the bounded chain cache without sending an Rc across threads.
        pub fn for_offload(&self) -> CertificateVerifier {
            CertificateVerifier {
                cluster: self.cluster.clone(),
                keys: (*self.keys).clone(),
                cache: self.cache.clone(),
            }
        }

        /// Reuse chain validation only while roots and time remain valid. The
        /// fingerprint must be a collision-resistant digest the caller computed
        /// from `chain` itself, never a value read from the wire.
        fn key(
            &self,
            fingerprint: &[u8; 32],
            chain: &[Vec<u8>],
            expected: &NodeId,
        ) -> Result<VerifyingKey> {
            let roots = self.keys.peer_trust_roots()?;
            let clock = self.keys.epochs.clock();
            let now = clock.unix_time().as_secs();
            let mut cache = self.cache.lock().map_err(|_| Error::Unavailable)?;
            cache.retain(|entry| {
                Arc::ptr_eq(&entry.roots, &roots) && now >= entry.checked && now < entry.until
            });
            if let Some(entry) = cache
                .iter()
                .find(|entry| &entry.node == expected && &entry.fingerprint == fingerprint)
            {
                return Ok(entry.key);
            }
            drop(cache);
            let verified = verify_chain(clock, &roots, chain, &self.cluster, expected)?;
            let mut cache = self.cache.lock().map_err(|_| Error::Unavailable)?;
            // At most 64 validated chain fingerprints.
            if cache.len() == 64 {
                cache.pop_front();
            }
            cache.push_back(CachedPeer {
                roots,
                fingerprint: *fingerprint,
                node: expected.clone(),
                key: verified.key,
                until: verified.expires,
                checked: now,
            });
            Ok(verified.key)
        }

        /// Exercise certificate acceptance without a message in component tests.
        #[cfg(test)]
        fn verify(&self, chain: &[Vec<u8>], expected: &NodeId) -> Result<VerifiedPeer> {
            if &self.cluster != self.keys.cluster() {
                return Err(Error::Unauthorized);
            }
            self.key(&chain_fingerprint(chain), chain, expected)?;
            Ok(VerifiedPeer {
                node: expected.clone(),
            })
        }

        /// Authenticate both the expected node and its signature over this message.
        pub fn verify_signed(
            &self,
            chain: &[Vec<u8>],
            expected: &NodeId,
            message: &[u8],
            signature: &[u8],
        ) -> Result<VerifiedPeer> {
            self.verify_signed_fingerprint(
                &chain_fingerprint(chain),
                chain,
                expected,
                message,
                signature,
            )
        }

        /// Like [`Self::verify_signed`], reusing validation by a fingerprint that the
        /// caller computed from `chain` with a collision-resistant digest.
        pub fn verify_signed_fingerprint(
            &self,
            fingerprint: &[u8; 32],
            chain: &[Vec<u8>],
            expected: &NodeId,
            message: &[u8],
            signature: &[u8],
        ) -> Result<VerifiedPeer> {
            if &self.cluster != self.keys.cluster() {
                return Err(Error::Unauthorized);
            }
            let key = self.key(fingerprint, chain, expected)?;
            key.verify_strict(message, signature)
                .map_err(|_| Error::Unauthorized)?;
            Ok(VerifiedPeer {
                node: expected.clone(),
            })
        }
    }

    /// Empty or coherent publication, with an independently installable identity.
    #[derive(Default)]
    struct State {
        published: Option<Arc<PublishedBundle>>,

        /// Secret-free admission index of `published`.
        index: Option<Arc<KeyIndex>>,

        identity: Option<InstalledIdentity>,
    }

    /// A complete publication; an uninstalled keyring has none of these fields.
    struct PublishedBundle {
        cluster: ClusterId,

        generation: BundleGeneration,

        fingerprint: [u8; 32],

        roots: Arc<Vec<Vec<u8>>>,

        entries: Vec<Arc<Entry>>,
    }

    /// Secret-free, sorted admission metadata for one published table. Worker
    /// views cache it; it never extends the lifetime of key material.
    struct KeyIndex {
        cluster: ClusterId,

        generation: BundleGeneration,

        entries: Vec<IndexEntry>,
    }

    /// One admitted epoch in [`KeyIndex`], sorted like the published entries.
    struct IndexEntry {
        reference: CacheKeyRef,

        state: CacheKeyState,

        entry: Weak<Entry>,
    }

    impl KeyIndex {
        /// Build the index for a published table.
        fn new(published: &PublishedBundle) -> Self {
            Self {
                cluster: published.cluster.clone(),
                generation: published.generation,
                entries: published
                    .entries
                    .iter()
                    .map(|entry| IndexEntry {
                        reference: entry.reference.clone(),
                        state: entry.state,
                        entry: Arc::downgrade(entry),
                    })
                    .collect(),
            }
        }

        /// Find an exact epoch.
        fn find(&self, cache: &CacheId, id: KeyId, purpose: KeyPurpose) -> Option<&IndexEntry> {
            let key = ((cache, purpose as u8), id.0);
            self.entries
                .binary_search_by(|entry| reference_order(&entry.reference).cmp(&key))
                .ok()
                .map(|i| &self.entries[i])
        }

        /// Find the unique active epoch of a cache and purpose.
        fn active(&self, cache: &CacheId, purpose: KeyPurpose) -> Option<&IndexEntry> {
            let group = (cache, purpose as u8);
            let first = self
                .entries
                .partition_point(|entry| reference_group(&entry.reference) < group);
            self.entries[first..]
                .iter()
                .take_while(|entry| reference_group(&entry.reference) == group)
                .find(|entry| entry.state == CacheKeyState::Active)
        }
    }

    /// An accepted signing identity and the trust snapshot that validated it.
    struct InstalledIdentity {
        identity: Arc<SigningIdentity>,

        roots: Arc<Vec<Vec<u8>>>,

        checked: u64,

        until: u64,
    }

    /// Validated, zeroizing input ready for a locked generation transition.
    struct ValidatedBundle<'a> {
        bundle: &'a KeyringBundle,

        fingerprint: [u8; 32],
    }

    /// Current admission metadata sharing immutable material with outstanding leases.
    struct Entry {
        reference: CacheKeyRef,

        state: CacheKeyState,

        secret: Arc<Secret>,
    }

    /// Wipes key material when its last admission or completion owner releases it.
    struct Secret {
        material: Zeroizing<[u8; 32]>,
        // XAES caches the master AES schedule and CMAC K1, not per-nonce GCM keys.
        page_cipher: crate::PageCipher,
    }

    impl Secret {
        fn new(material: Zeroizing<[u8; 32]>) -> Self {
            Self {
                page_cipher: crate::PageCipher::new(&material),
                material,
            }
        }
    }

    impl KeyLease {
        /// Validate purpose before application admission and integrity checks.
        pub fn require_purpose(&self, purpose: KeyPurpose) -> Result<()> {
            self.material(purpose).map(|_| ())
        }

        /// Enforce the immutable cache, key ID, and purpose before using the secret.
        fn bound_material(
            &self,
            cache: &CacheId,
            id: KeyId,
            purpose: KeyPurpose,
        ) -> Result<&[u8; 32]> {
            let material = self.material(purpose)?;
            if self.cache() != cache || self.id() != id {
                return Err(Error::MissingKey);
            }
            Ok(material)
        }

        /// Seal a page without copying input or consulting current epoch admission.
        /// Caller supplies canonical AAD and a fresh nonce for this key.
        pub fn seal_page(
            &self,
            cache: &CacheId,
            nonce: &[u8; 24],
            aad: &[u8],
            input: &[u8],
            out: &mut [u8],
        ) -> Result<()> {
            self.bound_material(cache, self.id(), KeyPurpose::Page)?;
            if !self.sealable {
                return Err(Error::MissingKey);
            }
            self.entry
                .secret
                .page_cipher
                .seal(nonce, aad, input, out)
                .map_err(|_| Error::CorruptRecord)
        }

        /// Open a page. Binding and length errors leave output untouched;
        /// authentication failure zeroes it (see [`crate::open`]).
        // Keep borrowed buffer and expected-identity arguments explicit at this boundary.
        #[allow(clippy::too_many_arguments)]
        pub fn open_page(
            &self,
            cache: &CacheId,
            id: KeyId,
            nonce: &[u8; 24],
            aad: &[u8],
            input: &[u8],
            out: &mut [u8],
        ) -> Result<()> {
            self.bound_material(cache, id, KeyPurpose::Page)?;
            self.entry
                .secret
                .page_cipher
                .open(nonce, aad, input, out)
                .map_err(|_| Error::CorruptRecord)
        }

        /// Open a page whose ciphertext body the caller already copied into
        /// `inout`, against its separate `tag`. Binding errors leave `inout`
        /// untouched; any other failure zeroes it.
        // Keep borrowed buffer and expected-identity arguments explicit at this boundary.
        #[allow(clippy::too_many_arguments)]
        pub fn open_page_in_place(
            &self,
            cache: &CacheId,
            id: KeyId,
            nonce: &[u8; 24],
            aad: &[u8],
            inout: &mut [u8],
            tag: &[u8],
        ) -> Result<()> {
            self.bound_material(cache, id, KeyPurpose::Page)?;
            self.entry
                .secret
                .page_cipher
                .open_in_place(nonce, aad, inout, tag)
                .map_err(|_| Error::CorruptRecord)
        }

        /// Verify a page tag without decrypting. `aad` parts are hashed as
        /// their concatenation. Checks cache, key ID and purpose like
        /// [`Self::open_page`] and accepts exactly what it would accept.
        // Keep borrowed buffer and expected-identity arguments explicit at this boundary.
        #[allow(clippy::too_many_arguments)]
        pub fn verify_page_tag(
            &self,
            cache: &CacheId,
            id: KeyId,
            nonce: &[u8; 24],
            aad: &[&[u8]],
            ciphertext: &[u8],
            tag: &[u8],
        ) -> Result<()> {
            self.bound_material(cache, id, KeyPurpose::Page)?;
            self.entry
                .secret
                .page_cipher
                .verify_tag(nonce, aad, ciphertext, tag)
                .map_err(|_| Error::CorruptRecord)
        }

        /// Derive the per-page AES-256-GCM key and nonce for offloaded page
        /// decryption (the GPU path). Checks cache, key ID and purpose like
        /// [`Self::open_page`]. Never exposes the epoch key itself; the
        /// derived key is valid for this one XAES nonce and wiped on drop.
        pub fn derive_page_subkey(
            &self,
            cache: &CacheId,
            id: KeyId,
            nonce: &[u8; 24],
        ) -> Result<crate::DerivedPageKey> {
            let material = self.bound_material(cache, id, KeyPurpose::Page)?;
            Ok(crate::xaes_derive(material, nonce))
        }

        /// Seal credentials using only a credential epoch. Caller owns nonce and AAD.
        pub fn seal_credentials(
            &self,
            cache: &CacheId,
            nonce: &[u8; 24],
            aad: &[u8],
            input: &[u8],
            out: &mut [u8],
        ) -> Result<()> {
            if !self.sealable {
                return Err(Error::MissingKey);
            }
            crate::seal_credentials(
                self.bound_material(cache, self.id(), KeyPurpose::OriginCredentials)?,
                nonce,
                aad,
                input,
                out,
            )
            .map_err(|_| Error::Unauthorized)
        }

        /// Authenticate credentials into borrowed output without staging plaintext.
        // Keep borrowed buffer and expected-identity arguments explicit at this boundary.
        #[allow(clippy::too_many_arguments)]
        pub fn open_credentials(
            &self,
            cache: &CacheId,
            id: KeyId,
            nonce: &[u8; 24],
            aad: &[u8],
            input: &[u8],
            out: &mut [u8],
        ) -> Result<()> {
            crate::open_credentials(
                self.bound_material(cache, id, KeyPurpose::OriginCredentials)?,
                nonce,
                aad,
                input,
                out,
            )
            .map_err(|_| Error::Unauthorized)
        }

        /// Derive and use the request-only MAC key internally; never export it.
        pub fn request_mac(
            &self,
            cache: &CacheId,
            message: &[u8],
            out: &mut [u8; 32],
        ) -> Result<()> {
            let material = self.bound_material(cache, self.id(), KeyPurpose::OriginCredentials)?;
            let mut domain = b"racer/request-mac/key/v1\0".to_vec();
            let length = u32::try_from(cache.0.len()).map_err(|_| Error::InvalidRequest)?;
            domain.extend_from_slice(&length.to_be_bytes());
            domain.extend_from_slice(cache.0.as_bytes());
            domain.extend_from_slice(&self.id().0);
            let derived = Zeroizing::new(crate::hmac_sha256(material, &domain));
            *out = crate::hmac_sha256(&derived, message);
            Ok(())
        }

        /// Authenticate a request MAC in constant time after checking its binding.
        pub fn verify_request_mac(
            &self,
            cache: &CacheId,
            id: KeyId,
            message: &[u8],
            tag: &[u8],
        ) -> Result<()> {
            self.bound_material(cache, id, KeyPurpose::OriginCredentials)?;
            let mut expected = [0; 32];
            self.request_mac(cache, message, &mut expected)?;
            if !crate::ct_eq(&expected, tag) {
                return Err(Error::Unauthorized);
            }
            Ok(())
        }

        /// Return the immutable generation-bound key ID.
        pub fn id(&self) -> KeyId {
            self.entry.reference.id
        }

        /// Return the cache authorized to use this secret.
        pub fn cache(&self) -> &CacheId {
            &self.entry.reference.cache
        }

        /// Exact immutable epoch identity, independent of current admission.
        pub fn reference(&self) -> &CacheKeyRef {
            &self.entry.reference
        }

        /// Borrow private material only for the lease's fixed purpose.
        fn material(&self, purpose: KeyPurpose) -> Result<&[u8; 32]> {
            if self.entry.reference.purpose != purpose {
                return Err(Error::MissingKey);
            }
            Ok(&self.entry.secret.material)
        }
    }

    impl Keyring {
        /// Locally installed coherent bundle, not a controller acknowledgment.
        pub fn generation(&self) -> Result<Option<u64>> {
            self.with_index(|index| Ok(index.map(|index| index.generation.0)))
        }

        /// Run `f` over the current admission index, reloading the local copy
        /// under the shared lock only when the published epoch has moved.
        #[inline]
        fn with_index<R>(&self, f: impl FnOnce(Option<&KeyIndex>) -> Result<R>) -> Result<R> {
            if self.epochs.epoch.load(Ordering::Acquire) != self.local_epoch.get() {
                self.reload()?;
            }
            f(self.local.borrow().as_deref())
        }

        /// Replace the local index copy with the shared one.
        #[cold]
        fn reload(&self) -> Result<()> {
            let state = self.epochs.state.lock().map_err(|_| Error::Unavailable)?;
            // The epoch only changes under this lock, so it names `index` exactly.
            let epoch = self.epochs.epoch.load(Ordering::Relaxed);
            let index = state.index.clone();
            drop(state);
            let old = self.local.replace(index);
            self.local_epoch.set(epoch);
            drop(old);
            Ok(())
        }

        /// Whether the current table admits this exact epoch.
        pub fn admits(&self, cache: &CacheId, id: KeyId, purpose: KeyPurpose) -> bool {
            self.with_index(|index| {
                Ok(index.is_some_and(|index| {
                    index.cluster == self.cluster && index.find(cache, id, purpose).is_some()
                }))
            })
            .unwrap_or(false)
        }

        /// Whether the current table has an active key for this cache and purpose.
        pub fn has_active(&self, cache: &CacheId, purpose: KeyPurpose) -> bool {
            self.with_index(|index| {
                Ok(index.is_some_and(|index| {
                    index.cluster == self.cluster && index.active(cache, purpose).is_some()
                }))
            })
            .unwrap_or(false)
        }

        /// Create a node-bound view without implicitly installing any keys.
        pub fn new(cluster: ClusterId, node: NodeId, epochs: Arc<KeyEpochs>) -> Self {
            Self {
                cluster,
                node,
                epochs,
                local_epoch: Cell::new(u64::MAX),
                local: RefCell::new(None),
            }
        }

        /// Return the cluster this view admits.
        pub fn cluster(&self) -> &ClusterId {
            &self.cluster
        }

        /// Return the node this view admits.
        pub fn node(&self) -> &NodeId {
            &self.node
        }

        /// Lease the current trust snapshot, preserving pointer identity on replay.
        pub fn peer_trust_roots(&self) -> Result<Arc<Vec<Vec<u8>>>> {
            let state = self.epochs.state.lock().map_err(|_| Error::Unavailable)?;
            let published = state.published.as_ref().ok_or(Error::MissingKey)?;
            if published.cluster != self.cluster {
                return Err(Error::MissingKey);
            }
            Ok(published.roots.clone())
        }

        /// Validate a wire transfer and atomically publish a complete key epoch.
        pub fn install(
            &self,
            bundle: racer_wire::control::KeyringBundle,
        ) -> Result<BundleGeneration> {
            // Bound private staging before allocating it, including direct callers
            // that do not pass through the wire decoder's byte limit.
            if bundle.cache_keys.len() > MAX_BUNDLE_KEYS {
                return Err(Error::InvalidConfiguration);
            }
            self.install_inner(&bundle.into())
        }

        /// Install, reporting replay as a configuration error.
        fn install_inner(&self, bundle: &KeyringBundle) -> Result<BundleGeneration> {
            self.install_checked(bundle).map_err(|error| match error {
                BundleError::Identity(error) => error,
                BundleError::Replay | BundleError::Wire(_) => Error::InvalidConfiguration,
            })
        }

        /// Prepare outside the lock, then validate the transition and publish once.
        fn install_checked(
            &self,
            bundle: &KeyringBundle,
        ) -> std::result::Result<BundleGeneration, BundleError> {
            let validated =
                ValidatedBundle::prepare(self.epochs.clock(), bundle, &self.cluster, &self.node)
                    .map_err(BundleError::Identity)?;
            let mut state = self
                .epochs
                .state
                .lock()
                .map_err(|_| BundleError::Identity(Error::Unavailable))?;
            let published = validated.transition(state.published.as_deref())?;
            let retired = published.map(|published| {
                state.index = Some(Arc::new(KeyIndex::new(&published)));
                // Release pairs with the Acquire load in `with_index`.
                self.epochs.epoch.fetch_add(1, Ordering::Release);
                state.published.replace(Arc::new(published))
            });
            drop(state);
            drop(retired);
            // Drop this view's copy of the old index now rather than on next use.
            self.reload().map_err(BundleError::Identity)?;
            Ok(bundle.generation)
        }
    }

    impl<'a> ValidatedBundle<'a> {
        /// Validate bounded input and fingerprint its original, order-sensitive form.
        fn prepare(
            clock: &WallClock,
            bundle: &'a KeyringBundle,
            cluster: &ClusterId,
            node: &NodeId,
        ) -> Result<Self> {
            if bundle.schema_version != KEYRING_SCHEMA_VERSION
                || &bundle.cluster != cluster
                || bundle.cache_keys.len() > MAX_BUNDLE_KEYS
                || bundle.generation.0 == 0
                || !canonical_uuid(&cluster.0)
                || !canonical_uuid(&node.0)
            {
                return Err(Error::InvalidConfiguration);
            }
            root_store(clock, &bundle.peer_trust_roots)?;
            let mut hash = Sha256::new();
            hash.update(b"racer/keyring/bundle/v1\0");
            hash.update((bundle.peer_trust_roots.len() as u64).to_be_bytes());
            for root in &bundle.peer_trust_roots {
                hash.update((root.len() as u64).to_be_bytes());
                hash.update(root);
            }
            hash.update((bundle.cache_keys.len() as u64).to_be_bytes());
            for key in &bundle.cache_keys {
                hash.update((key.key.cache.0.len() as u64).to_be_bytes());
                hash.update(key.key.cache.0.as_bytes());
                hash.update(key.key.id.0);
                hash.update([key.key.purpose as u8, key.state as u8]);
                hash.update(key.material.as_slice());
            }
            let fingerprint: [u8; 32] = hash.finalize().into();
            // Validate the downloaded epoch before acquiring the shared publication
            // lock. Existing immutable secret leases remain usable during validation.
            // Hashed checks keep validation O(n) for the largest catalogs.
            let n = bundle.cache_keys.len();
            let mut references = HashSet::with_capacity(n);
            let mut materials = HashSet::with_capacity(n);
            // Per cache and purpose: prepared, active, and decrypt counts.
            let mut groups: HashMap<(&CacheId, u8), [usize; 3]> = HashMap::with_capacity(n);
            for candidate in &bundle.cache_keys {
                let generation = candidate
                    .key
                    .id
                    .generation()
                    .ok_or(Error::InvalidConfiguration)?;
                if generation > bundle.generation.0
                    || !canonical_uuid(&candidate.key.cache.0)
                    || !references.insert(reference_order(&candidate.key))
                    || !materials.insert(&*candidate.material)
                {
                    return Err(Error::InvalidConfiguration);
                }
                let counts = groups.entry(reference_group(&candidate.key)).or_default();
                counts[match candidate.state {
                    CacheKeyState::Prepared => 0,
                    CacheKeyState::Active => 1,
                    CacheKeyState::Decrypt => 2,
                }] += 1;
            }
            for ((_, purpose), [prepared, active, decrypt]) in groups {
                let max_decrypt = if purpose == KeyPurpose::Page as u8 {
                    MAX_RETAINED_PAGE_KEYS - 1
                } else {
                    0
                };
                if active != 1 || prepared > 1 || decrypt > max_decrypt {
                    return Err(Error::InvalidConfiguration);
                }
            }
            Ok(Self {
                bundle,
                fingerprint,
            })
        }

        /// Check the current epoch and construct a replacement without mutating it.
        /// Exact replay returns no replacement, preserving all shared owner identities.
        fn transition(
            self,
            current: Option<&PublishedBundle>,
        ) -> std::result::Result<Option<PublishedBundle>, BundleError> {
            let Self {
                bundle,
                fingerprint,
            } = self;
            if accept_bundle(current, bundle, &fingerprint)? == Accepted::Exact {
                return Ok(None);
            }
            self::transition_entries(bundle, fingerprint, current).map_err(BundleError::Identity)
        }
    }

    /// How a delivered bundle relates to the published one.
    #[derive(Clone, Copy, Debug, Eq, PartialEq)]
    enum Accepted {
        /// The exact published bundle was delivered again.
        Exact,

        /// A newer generation (or the first one).
        Newer,
    }

    /// The single replay and monotonicity check for keyring bundles: a
    /// different cluster is invalid; an older generation, or the same
    /// generation with different content, is a replay.
    fn accept_bundle(
        current: Option<&PublishedBundle>,
        bundle: &KeyringBundle,
        fingerprint: &[u8; 32],
    ) -> std::result::Result<Accepted, BundleError> {
        let Some(current) = current else {
            return Ok(Accepted::Newer);
        };
        if current.cluster != bundle.cluster {
            return Err(BundleError::Identity(Error::InvalidConfiguration));
        }
        if bundle.generation < current.generation
            || bundle.generation == current.generation && current.fingerprint != *fingerprint
        {
            return Err(BundleError::Replay);
        }
        Ok(if bundle.generation == current.generation {
            Accepted::Exact
        } else {
            Accepted::Newer
        })
    }

    /// Build a replacement table for a newer generation without mutating the
    /// current one.
    fn transition_entries(
        bundle: &KeyringBundle,
        fingerprint: [u8; 32],
        current: Option<&PublishedBundle>,
    ) -> Result<Option<PublishedBundle>> {
        {
            if let Some(current) = current {
                let by_reference: HashMap<_, &Arc<Entry>> = current
                    .entries
                    .iter()
                    .map(|e| (reference_order(&e.reference), e))
                    .collect();
                let by_material: HashMap<&[u8; 32], &CacheKeyRef> = current
                    .entries
                    .iter()
                    .map(|e| (&*e.secret.material, &e.reference))
                    .collect();
                for candidate in &bundle.cache_keys {
                    let generation = candidate
                        .key
                        .id
                        .generation()
                        .ok_or(Error::InvalidConfiguration)?;
                    let old = by_reference.get(&reference_order(&candidate.key));
                    // Removed IDs cannot reenter admission, even as prepared keys.
                    if generation <= current.generation.0 && old.is_none() {
                        return Err(Error::InvalidConfiguration);
                    }
                    if old.is_some_and(|old| *old.secret.material != *candidate.material) {
                        return Err(Error::InvalidConfiguration);
                    }
                    if by_material
                        .get(&*candidate.material)
                        .is_some_and(|r| **r != candidate.key)
                    {
                        return Err(Error::InvalidConfiguration);
                    }
                }
            }
            // Removed entries need no tombstone or polling state. Accepted jobs own
            // their Arc independently, including jobs whose caller has timed out.
            let mut entries: Vec<_> = bundle
                .cache_keys
                .iter()
                .map(|candidate| {
                    let retained = current.and_then(|current| {
                        current
                            .entries
                            .binary_search_by(|e| {
                                reference_order(&e.reference).cmp(&reference_order(&candidate.key))
                            })
                            .ok()
                            .map(|i| &current.entries[i])
                    });
                    Arc::new(Entry {
                        reference: candidate.key.clone(),
                        state: candidate.state,
                        secret: retained.map_or_else(
                            || Arc::new(Secret::new(candidate.material.clone())),
                            |entry| entry.secret.clone(),
                        ),
                    })
                })
                .collect();
            entries
                .sort_by(|a, b| reference_order(&a.reference).cmp(&reference_order(&b.reference)));
            let roots = current
                .filter(|current| *current.roots == bundle.peer_trust_roots)
                .map_or_else(
                    || Arc::new(bundle.peer_trust_roots.clone()),
                    |current| current.roots.clone(),
                );
            Ok(Some(PublishedBundle {
                cluster: bundle.cluster.clone(),
                generation: bundle.generation,
                fingerprint,
                roots,
                entries,
            }))
        }
    }

    impl Keyring {
        /// Accept a matching signing identity against roots unchanged by validation.
        pub fn install_signing_identity(&self, identity: Arc<SigningIdentity>) -> Result<()> {
            if identity.node() != &self.node || identity.cluster() != &self.cluster {
                return Err(Error::Unauthorized);
            }
            let roots = self.peer_trust_roots()?;
            let verified = verify_chain(
                self.epochs.clock(),
                &roots,
                identity.certificate_chain(),
                &self.cluster,
                &self.node,
            )?;
            let checked = self.epochs.clock().unix_time().as_secs();
            let mut state = self.epochs.state.lock().map_err(|_| Error::Unavailable)?;
            if !state
                .published
                .as_ref()
                .is_some_and(|bundle| Arc::ptr_eq(&roots, &bundle.roots))
            {
                return Err(Error::Unavailable);
            }
            state.identity = Some(InstalledIdentity {
                identity,
                roots,
                checked,
                until: verified.expires,
            });
            Ok(())
        }

        /// Lease the identity after checking current trust and clock continuity.
        pub fn signing_identity(&self) -> Result<Arc<SigningIdentity>> {
            let state = self.epochs.state.lock().map_err(|_| Error::Unavailable)?;
            let installed = state.identity.as_ref().ok_or(Error::MissingKey)?;
            let identity = &installed.identity;
            if identity.node() != &self.node || identity.cluster() != &self.cluster {
                return Err(Error::Unauthorized);
            }
            let now = self.epochs.clock().unix_time().as_secs();
            let identity = identity.clone();
            let published = state.published.as_ref().ok_or(Error::MissingKey)?;
            if Arc::ptr_eq(&installed.roots, &published.roots)
                && now >= installed.checked
                && now < installed.until
            {
                return Ok(identity);
            }
            let roots = published.roots.clone();
            drop(state);
            let verified = verify_chain(
                self.epochs.clock(),
                &roots,
                identity.certificate_chain(),
                &self.cluster,
                &self.node,
            )?;
            let mut state = self.epochs.state.lock().map_err(|_| Error::Unavailable)?;
            if !state
                .published
                .as_ref()
                .is_some_and(|bundle| Arc::ptr_eq(&roots, &bundle.roots))
                || !state
                    .identity
                    .as_ref()
                    .is_some_and(|current| Arc::ptr_eq(&current.identity, &identity))
            {
                return Err(Error::Unavailable);
            }
            state.identity = Some(InstalledIdentity {
                identity: identity.clone(),
                roots,
                checked: now,
                until: verified.expires,
            });
            Ok(identity)
        }

        /// Admit a specific active, prepared, or decrypt epoch for its cache and
        /// purpose. Only a lease of a key that was active when leased can seal.
        pub fn lease(
            &self,
            cache: Option<&CacheId>,
            id: KeyId,
            purpose: KeyPurpose,
        ) -> Result<KeyLease> {
            let cache = cache.ok_or(Error::MissingKey)?;
            self.lease_with(|index| index.find(cache, id, purpose))
        }

        /// Admit the unique active key for this cache and purpose.
        pub fn active(&self, cache: &CacheId, purpose: KeyPurpose) -> Result<KeyLease> {
            self.lease_with(|index| index.active(cache, purpose))
        }

        /// Lease the entry selected from the current index without a lock or
        /// an allocation. A concurrent replacement can retire the entry between
        /// the epoch check and the upgrade; then reload once and retry.
        fn lease_with(
            &self,
            select: impl Fn(&KeyIndex) -> Option<&IndexEntry>,
        ) -> Result<KeyLease> {
            for _ in 0..2 {
                let leased = self.with_index(|index| {
                    let index = index.ok_or(Error::MissingKey)?;
                    if index.cluster != self.cluster {
                        return Err(Error::MissingKey);
                    }
                    let found = select(index).ok_or(Error::MissingKey)?;
                    Ok(found.entry.upgrade().map(|entry| KeyLease {
                        entry,
                        sealable: found.state == CacheKeyState::Active,
                    }))
                })?;
                if let Some(lease) = leased {
                    return Ok(lease);
                }
                self.reload()?;
            }
            Err(Error::MissingKey)
        }
    }

    impl std::fmt::Display for Error {
        /// Render the payload-free variant name.
        fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
            write!(f, "{self:?}")
        }
    }

    impl std::error::Error for Error {}

    /// Order cache/purpose groups identically for sorting and active admission.
    fn reference_group(reference: &CacheKeyRef) -> (&CacheId, u8) {
        (&reference.cache, reference.purpose as u8)
    }

    /// Order exact epoch identities identically for publication and binary search.
    fn reference_order(reference: &CacheKeyRef) -> ((&CacheId, u8), [u8; 16]) {
        (reference_group(reference), reference.id.0)
    }

    /// Validate a node client-auth chain against roots at the clock's current time.
    pub fn verify_chain(
        clock: &WallClock,
        roots: &[Vec<u8>],
        chain: &[Vec<u8>],
        cluster: &ClusterId,
        node: &NodeId,
    ) -> Result<ValidatedChain> {
        if chain.is_empty()
            || chain.len() > 8
            || chain.iter().any(|c| c.is_empty() || c.len() > 16384)
            || chain.iter().map(Vec::len).sum::<usize>() > 65536
        {
            return Err(Error::Unauthorized);
        }
        let verifier = rustls::server::WebPkiClientVerifier::builder_with_provider(
            Arc::new(root_store(clock, roots)?),
            Arc::new(rustls::crypto::ring::default_provider()),
        )
        .build()
        .map_err(|_| Error::Unauthorized)?;
        let leaf = CertificateDer::from(chain[0].as_slice());
        let intermediates: Vec<_> = chain[1..]
            .iter()
            .map(|c| CertificateDer::from(c.as_slice()))
            .collect();
        verifier
            .verify_client_cert(&leaf, &intermediates, clock.unix_time())
            .map_err(|_| Error::Unauthorized)?;
        let (rest, cert) = parse_x509_certificate(&chain[0]).map_err(|_| Error::Unauthorized)?;
        if !rest.is_empty()
            || cert.is_ca()
            || cert.public_key().algorithm.algorithm.to_id_string() != "1.3.101.112"
            || cert.public_key().algorithm.parameters.is_some()
        {
            return Err(Error::Unauthorized);
        }
        let san = cert
            .subject_alternative_name()
            .map_err(|_| Error::Unauthorized)?
            .ok_or(Error::Unauthorized)?;
        let usage = cert
            .key_usage()
            .map_err(|_| Error::Unauthorized)?
            .ok_or(Error::Unauthorized)?;
        let extended = cert
            .extended_key_usage()
            .map_err(|_| Error::Unauthorized)?
            .ok_or(Error::Unauthorized)?;
        if !usage.value.digital_signature()
            || usage.value.key_cert_sign()
            || !extended.value.client_auth
        {
            return Err(Error::Unauthorized);
        }
        let expected = spiffe(cluster, node)?;
        let uris: Vec<_> = san
            .value
            .general_names
            .iter()
            .filter_map(|n| {
                if let GeneralName::URI(s) = n {
                    Some(*s)
                } else {
                    None
                }
            })
            .collect();
        if uris != [expected.as_str()] {
            return Err(Error::Unauthorized);
        }
        let key: &[u8; 32] = cert
            .public_key()
            .subject_public_key
            .data
            .as_ref()
            .try_into()
            .map_err(|_| Error::Unauthorized)?;
        let key = VerifyingKey::from_bytes(key).map_err(|_| Error::Unauthorized)?;
        let (valid_from, expires) = validity(chain.iter().chain(roots.iter()))?;
        Ok(ValidatedChain {
            key,
            valid_from,
            expires,
        })
    }

    /// Parse bounded CA roots and require every configured root to be valid now.
    pub fn root_store(clock: &WallClock, roots: &[Vec<u8>]) -> Result<RootCertStore> {
        if roots.is_empty() || roots.len() > 32 {
            return Err(Error::Unauthorized);
        }
        let mut store = RootCertStore::empty();
        for root in roots {
            if root.is_empty() || root.len() > 16384 {
                return Err(Error::Unauthorized);
            }
            let (rest, cert) = parse_x509_certificate(root).map_err(|_| Error::Unauthorized)?;
            let now = clock.unix_time().as_secs();
            let now = i64::try_from(now).map_err(|_| Error::Unauthorized)?;
            let now = x509_parser::time::ASN1Time::from_timestamp(now)
                .map_err(|_| Error::Unauthorized)?;
            if !rest.is_empty() || !cert.is_ca() || !cert.validity().is_valid_at(now) {
                return Err(Error::Unauthorized);
            }
            store
                .add(CertificateDer::from(root.clone()))
                .map_err(|_| Error::Unauthorized)?;
        }
        Ok(store)
    }

    /// Conservative validity intersection includes all configured trust anchors.
    fn validity<'a>(certificates: impl Iterator<Item = &'a Vec<u8>>) -> Result<(u64, u64)> {
        let mut start = 0;
        let mut end = u64::MAX;
        for der in certificates {
            let (_, cert) = parse_x509_certificate(der).map_err(|_| Error::Unauthorized)?;
            start = start.max(cert.validity().not_before.timestamp().max(0) as u64);
            end = end.min(
                u64::try_from(cert.validity().not_after.timestamp())
                    .map_err(|_| Error::Unauthorized)?,
            );
        }
        Ok((start, end))
    }

    /// Build the single canonical URI permitted for this cluster and node.
    fn spiffe(cluster: &ClusterId, node: &NodeId) -> Result<String> {
        if !canonical_uuid(&cluster.0) || !canonical_uuid(&node.0) {
            return Err(Error::Unauthorized);
        }
        Ok(format!("spiffe://{}/node/{}", cluster.0, node.0))
    }

    /// Opt-in Ed25519 certificate fixtures with explicit parameter customization.
    #[cfg(any(test, feature = "test-util"))]
    pub mod test_util {
        use super::*;

        /// Generate an unconstrained signing CA using the fixture defaults.
        pub fn ca() -> (rcgen::Certificate, rcgen::KeyPair) {
            let mut params = rcgen::CertificateParams::new(Vec::<String>::new()).unwrap();
            params.is_ca = rcgen::IsCa::Ca(rcgen::BasicConstraints::Unconstrained);
            params.key_usages = vec![
                rcgen::KeyUsagePurpose::KeyCertSign,
                rcgen::KeyUsagePurpose::CrlSign,
            ];
            let key = rcgen::KeyPair::generate_for(&rcgen::PKCS_ED25519).unwrap();
            (params.self_signed(&key).unwrap(), key)
        }

        /// Generate a node key and issue a customizable client-auth certificate.
        pub fn issue(
            ca: &rcgen::Certificate,
            ca_key: &rcgen::KeyPair,
            cluster: &ClusterId,
            node: &NodeId,
            customize: impl FnOnce(&mut rcgen::CertificateParams),
        ) -> (PendingIdentity, Vec<Vec<u8>>) {
            let pending = PendingIdentity::generate().unwrap();
            issue_pending(pending, ca, ca_key, cluster, node, customize)
        }

        /// Issue for an existing key without regenerating it or changing defaults.
        pub fn issue_pending(
            pending: PendingIdentity,
            ca: &rcgen::Certificate,
            ca_key: &rcgen::KeyPair,
            cluster: &ClusterId,
            node: &NodeId,
            customize: impl FnOnce(&mut rcgen::CertificateParams),
        ) -> (PendingIdentity, Vec<Vec<u8>>) {
            let secret = pending.export_pkcs8_for_persistence().unwrap();
            let key = rcgen::KeyPair::from_pkcs8_der_and_sign_algo(
                &PrivatePkcs8KeyDer::from(secret.as_slice()),
                &rcgen::PKCS_ED25519,
            )
            .unwrap();
            let mut params = rcgen::CertificateParams::new(Vec::<String>::new()).unwrap();
            params.subject_alt_names = vec![rcgen::SanType::URI(
                format!("spiffe://{}/node/{}", cluster.0, node.0)
                    .try_into()
                    .unwrap(),
            )];
            params.key_usages = vec![rcgen::KeyUsagePurpose::DigitalSignature];
            params.extended_key_usages = vec![rcgen::ExtendedKeyUsagePurpose::ClientAuth];
            customize(&mut params);
            let cert = params.signed_by(&key, ca, ca_key).unwrap();
            (pending, vec![cert.der().to_vec()])
        }
    }

    /// Canonical bundle delivery, rejection, and owner replacement contracts.
    #[cfg(test)]
    mod bundle_tests {
        use super::test_util::*;
        use super::*;

        /// Cached delivery cannot return stale roots after another view advances epochs.
        #[test]
        fn cached_delivery_revalidates_shared_keyring_epoch() {
            let cluster = ClusterId("11111111-1111-4111-8111-111111111111".into());
            let node = NodeId("22222222-2222-4222-8222-222222222222".into());
            let epochs = Arc::new(KeyEpochs::default());
            let keys = Rc::new(Keyring::new(cluster.clone(), node.clone(), epochs.clone()));
            let other = Keyring::new(cluster.clone(), node, epochs);
            let installer = BundleInstaller::new(keys.clone());
            let (old_ca, _) = ca();
            let (new_ca, _) = ca();
            let old = racer_wire::control::KeyringBundle {
                schema_version: racer_wire::control::KEYRING_SCHEMA_VERSION,
                cluster,
                generation: BundleGeneration(2),
                peer_trust_roots: vec![old_ca.der().to_vec()],
                cache_keys: vec![],
            };
            let accepted = installer.install(old.clone()).unwrap();
            let original_roots = keys.peer_trust_roots().unwrap();
            assert_eq!(installer.install(old.clone()).unwrap(), accepted);
            assert!(Arc::ptr_eq(
                &keys.peer_trust_roots().unwrap(),
                &original_roots
            ));
            let mut newer = old.clone();
            newer.generation = BundleGeneration(3);
            newer.peer_trust_roots = vec![new_ca.der().to_vec()];
            other.install(newer.clone()).unwrap();
            assert_eq!(keys.generation().unwrap(), Some(3));
            // Rollback below the shared epoch is a replay.
            assert_eq!(installer.install(old), Err(BundleError::Replay));
            assert_eq!(*keys.peer_trust_roots().unwrap(), newer.peer_trust_roots);
            assert_eq!(installer.generation(), Some(BundleGeneration(2)));
            assert_eq!(
                installer.install(newer.clone()).unwrap(),
                (BundleGeneration(3), newer.peer_trust_roots)
            );
            assert_eq!(installer.generation(), Some(BundleGeneration(3)));
        }

        /// Canonical delivery is idempotent, rejects rollback, and resets with its keyring.
        #[test]
        fn bundle_delivery_canonical_replay_and_rebinding() {
            let cluster = ClusterId("11111111-1111-4111-8111-111111111111".into());
            let node = NodeId("22222222-2222-4222-8222-222222222222".into());
            let (first, first_key) = ca();
            let (second, _) = ca();
            let keys = || {
                Rc::new(Keyring::new(
                    cluster.clone(),
                    node.clone(),
                    Arc::new(KeyEpochs::default()),
                ))
            };
            let installer = BundleInstaller::new(keys());
            assert_eq!(installer.generation(), None);
            let mut bundle = racer_wire::control::KeyringBundle {
                schema_version: racer_wire::control::KEYRING_SCHEMA_VERSION,
                cluster,
                generation: BundleGeneration(2),
                peer_trust_roots: vec![first.der().to_vec(), second.der().to_vec()],
                cache_keys: vec![],
            };
            let accepted = installer.install(bundle.clone()).unwrap();
            bundle.peer_trust_roots.reverse();
            assert_eq!(installer.install(bundle.clone()).unwrap(), accepted);
            let mut conflict = bundle.clone();
            conflict.peer_trust_roots.pop();
            assert_eq!(installer.install(conflict), Err(BundleError::Replay));
            bundle.generation = BundleGeneration(1);
            assert_eq!(installer.install(bundle.clone()), Err(BundleError::Replay));
            assert_eq!(installer.generation(), Some(BundleGeneration(2)));
            let replacement = Rc::new(Keyring::new(
                bundle.cluster.clone(),
                node,
                Arc::new(KeyEpochs::default()),
            ));
            installer.bind_keyring(replacement);
            assert_eq!(installer.generation(), None);
            assert_eq!(
                installer.install(bundle.clone()).unwrap().0,
                BundleGeneration(1)
            );
            bundle.generation = BundleGeneration(3);
            bundle.peer_trust_roots = vec![vec![0; 128]];
            // Malformed roots fail trust validation (no wire re-encoding).
            assert_eq!(
                installer.install(bundle.clone()),
                Err(BundleError::Identity(Error::Unauthorized))
            );
            let (_, chain) = issue(
                &first,
                &first_key,
                &bundle.cluster,
                &NodeId("22222222-2222-4222-8222-222222222222".into()),
                |_| {},
            );
            bundle.peer_trust_roots = chain;
            assert_eq!(
                installer.install(bundle),
                Err(BundleError::Identity(Error::Unauthorized))
            );
            assert_eq!(installer.generation(), Some(BundleGeneration(1)));
        }
    }

    /// Certificate rejection and cache invalidation state-space tests.
    #[cfg(test)]
    mod certificate_tests {
        use super::tests::{CLUSTER, NODE, issued};
        use super::*;

        /// Cached chains still require current roots, time, exact bytes, and signatures.
        #[test]
        fn cache_requires_exact_chain_current_trust_time_and_fresh_signature() {
            let wall = Arc::new(Mutex::new(std::time::SystemTime::now()));
            let clock = {
                let wall = wall.clone();
                WallClock::new(move || *wall.lock().unwrap())
            };
            let (pending, chain, roots) = issued();
            let keys = Rc::new(Keyring::new(
                ClusterId(CLUSTER.into()),
                NodeId(NODE.into()),
                Arc::new(KeyEpochs::with_clock(clock.clone())),
            ));
            let bundle = |generation, roots| racer_wire::control::KeyringBundle {
                schema_version: racer_wire::control::KEYRING_SCHEMA_VERSION,
                cluster: ClusterId(CLUSTER.into()),
                generation: BundleGeneration(generation),
                peer_trust_roots: roots,
                cache_keys: vec![],
            };
            keys.install(bundle(1, roots.clone())).unwrap();
            let identity = pending
                .accept(
                    ClusterId(CLUSTER.into()),
                    NodeId(NODE.into()),
                    chain.clone(),
                    &roots,
                    &clock,
                )
                .unwrap();
            let certs = Certificates::new(ClusterId(CLUSTER.into()), keys.clone());
            let node = NodeId(NODE.into());
            let signature = identity.sign(b"message").unwrap();
            for _ in 0..2 {
                certs
                    .verify_signed(&chain, &node, b"message", &signature)
                    .unwrap();
            }
            assert!(
                certs
                    .verify_signed(&chain, &node, b"tampered", &signature)
                    .is_err()
            );
            let mut changed = chain.clone();
            changed[0].push(0);
            assert!(certs.verify(&changed, &node).is_err());
            let original = clock.now();
            let (_, until) = validity(chain.iter().chain(roots.iter())).unwrap();
            *wall.lock().unwrap() =
                std::time::SystemTime::UNIX_EPOCH + std::time::Duration::from_secs(until + 1);
            assert!(certs.verify(&chain, &node).is_err());
            *wall.lock().unwrap() = original;
            certs.verify(&chain, &node).unwrap();
            let (_, _, replacement) = issued();
            keys.install(bundle(2, replacement)).unwrap();
            assert!(certs.verify(&chain, &node).is_err());
        }

        /// Reject alternate spellings that could alias a canonical node identity.
        #[test]
        fn canonical_identity_rejects_aliases() {
            assert!(canonical_uuid(CLUSTER));
            for value in [
                "abc",
                "AAAAAAAA-1111-4111-8111-111111111111",
                "11111111111141118111111111111111",
                "11111111-1111-4111-8111-11111111111/",
            ] {
                assert!(!canonical_uuid(value));
                assert_eq!(
                    spiffe(&ClusterId(value.into()), &NodeId(NODE.into())),
                    Err(Error::Unauthorized)
                );
                assert_eq!(
                    spiffe(&ClusterId(CLUSTER.into()), &NodeId(value.into())),
                    Err(Error::Unauthorized)
                );
            }
        }
    }

    /// Epoch transition tests cover rejection, replay, and retained ownership.
    #[cfg(test)]
    mod keyring_tests {
        use super::tests::{CACHE, CLUSTER, NODE};
        use super::*;

        /// Decode a hex test fixture. Fixtures are decoded at runtime so test nonces
        /// are not mistaken for hard-coded production crypto values.
        fn hex_fixture<const N: usize>(hex: &str) -> [u8; N] {
            let digit = |byte: u8| (byte as char).to_digit(16).unwrap() as u8;
            let bytes: Vec<u8> = hex
                .as_bytes()
                .chunks_exact(2)
                .map(|pair| digit(pair[0]) << 4 | digit(pair[1]))
                .collect();
            bytes.try_into().unwrap()
        }

        /// Install the default cache's page and credential keys.
        fn keys() -> Keyring {
            let (_, _, roots) = super::tests::issued();
            let keys = Keyring::new(
                ClusterId(CLUSTER.into()),
                NodeId(NODE.into()),
                Arc::new(KeyEpochs::default()),
            );
            keys.install_inner(&bundle(1, roots, CacheKeyState::Active))
                .unwrap();
            keys
        }

        /// Create fresh generation-bound IDs and material for a rotation.
        fn rotation_bundle(generation: u64, roots: Vec<Vec<u8>>) -> KeyringBundle {
            let mut next = bundle(generation, roots, CacheKeyState::Active);
            for (i, key) in next.cache_keys.iter_mut().enumerate() {
                key.key.id.0[..4].copy_from_slice(b"RKG1");
                key.key.id.0[4..12].copy_from_slice(&generation.to_be_bytes());
                key.key.id.0[12..].copy_from_slice(&(i as u32).to_be_bytes());
                key.material[..8].copy_from_slice(&generation.to_be_bytes());
            }
            next
        }

        /// Inspect current admission without counting outstanding lease owners.
        fn admitted_count(keys: &Keyring) -> usize {
            keys.epochs
                .state
                .lock()
                .unwrap()
                .published
                .as_ref()
                .unwrap()
                .entries
                .len()
        }

        /// Replay is order-sensitive; failures preserve roots and retained secret owners.
        #[test]
        fn publication_is_coherent_and_replay_preserves_shared_owners() {
            let (_, _, mut roots) = super::tests::issued();
            roots.extend(super::tests::issued().2);
            let keys = Keyring::new(
                ClusterId(CLUSTER.into()),
                NodeId(NODE.into()),
                Arc::new(KeyEpochs::default()),
            );
            let mut initial = bundle(1, roots, CacheKeyState::Active);
            let mut invalid = initial.clone();
            invalid.cache_keys[1].material = invalid.cache_keys[0].material.clone();
            assert_eq!(
                keys.install_inner(&invalid),
                Err(Error::InvalidConfiguration)
            );
            assert!(keys.epochs.state.lock().unwrap().published.is_none());
            assert_eq!(keys.generation().unwrap(), None);
            assert!(keys.peer_trust_roots().is_err());

            keys.install_inner(&initial).unwrap();
            let roots = keys.peer_trust_roots().unwrap();
            let held = keys
                .active(&CacheId(CACHE.into()), KeyPurpose::Page)
                .unwrap();
            keys.install_inner(&initial).unwrap();
            for reorder_roots in [false, true] {
                let mut changed = initial.clone();
                if reorder_roots {
                    changed.peer_trust_roots.reverse();
                } else {
                    changed.cache_keys.reverse();
                }
                assert_eq!(
                    keys.install_inner(&changed),
                    Err(Error::InvalidConfiguration)
                );
                assert_eq!(keys.generation().unwrap(), Some(1));
                assert!(Arc::ptr_eq(&roots, &keys.peer_trust_roots().unwrap()));
                let admitted = keys.active(held.cache(), KeyPurpose::Page).unwrap();
                assert!(Arc::ptr_eq(&held.entry.secret, &admitted.entry.secret));
            }

            initial.generation = BundleGeneration(2);
            keys.install_inner(&initial).unwrap();
            assert_eq!(keys.generation().unwrap(), Some(2));
            assert!(Arc::ptr_eq(&roots, &keys.peer_trust_roots().unwrap()));
            assert!(Arc::ptr_eq(
                &held.entry.secret,
                &keys
                    .active(held.cache(), KeyPurpose::Page)
                    .unwrap()
                    .entry
                    .secret
            ));
        }

        /// Views share one published table but cache only a secret-free index:
        /// an idle view never extends key material, and sees rotation lazily.
        #[test]
        fn view_index_follows_epoch_without_pinning_secrets() {
            let keys = keys();
            let other = Keyring::new(
                keys.cluster().clone(),
                keys.node().clone(),
                keys.epochs.clone(),
            );
            let roots = (*keys.peer_trust_roots().unwrap()).clone();
            let first = rotation_bundle(2, roots.clone());
            keys.install_inner(&first).unwrap();
            let cache = CacheId(CACHE.into());
            let lease = other.active(&cache, KeyPurpose::Page).unwrap();
            let held = lease.reference().clone();
            assert!(other.admits(&cache, held.id, KeyPurpose::Page));
            assert!(other.has_active(&cache, KeyPurpose::Page));
            let secret = Arc::downgrade(&lease.entry.secret);
            keys.install_inner(&rotation_bundle(100, roots)).unwrap();
            // `other` has not looked since; its stale index must not pin secrets.
            drop(lease);
            assert!(secret.upgrade().is_none());
            assert!(!other.admits(&cache, held.id, KeyPurpose::Page));
            assert!(
                other
                    .lease(Some(&cache), held.id, KeyPurpose::Page)
                    .is_err()
            );
            assert_eq!(other.generation().unwrap(), Some(100));
        }

        /// Removed epochs cannot return even after skipped bundle generations.
        #[test]
        fn generation_bound_ids_reject_resurrection_skips_future_and_zero_epochs() {
            let keys = keys();
            let roots = (*keys.peer_trust_roots().unwrap()).clone();
            let first = rotation_bundle(2, roots.clone());
            keys.install_inner(&first).unwrap();
            let lease = keys
                .active(&CacheId(CACHE.into()), KeyPurpose::Page)
                .unwrap();
            let secret = Arc::downgrade(&lease.entry.secret);
            // Skipped bundle generations are normal after projected-secret delays.
            let next = rotation_bundle(100, roots.clone());
            keys.install_inner(&next).unwrap();
            let held = first.cache_keys[0].key.clone();
            assert_eq!(secret.strong_count(), 1);
            drop(lease);
            assert!(secret.upgrade().is_none());
            assert!(
                keys.lease(Some(&held.cache), held.id, KeyPurpose::Page)
                    .is_err()
            );
            for generation in [0, 2, 99, 100, 102] {
                let mut bad = rotation_bundle(generation, roots.clone());
                bad.generation = BundleGeneration(101);
                bad.cache_keys[0].key.id.0[15] ^= 128;
                assert_eq!(keys.install_inner(&bad), Err(Error::InvalidConfiguration));
            }
            let mut resurrected = first.clone();
            resurrected.generation = BundleGeneration(101);
            assert_eq!(
                keys.install_inner(&resurrected),
                Err(Error::InvalidConfiguration)
            );
            assert_eq!(keys.install_inner(&first), Err(Error::InvalidConfiguration));
            assert_eq!(keys.install_inner(&next), Ok(BundleGeneration(100)));
            assert_eq!(admitted_count(&keys), 2);
            let mut overlap = rotation_bundle(101, roots.clone());
            let removed = rotation_bundle(100, roots).cache_keys;
            keys.install_inner(&overlap).unwrap();
            let old = &removed[0].key;
            assert_eq!(keys.install_inner(&overlap), Ok(BundleGeneration(101)));
            assert!(
                keys.lease(Some(&old.cache), old.id, KeyPurpose::Page)
                    .is_err()
            );
            // Later bundles and exact replays continue to omit removed epochs.
            overlap.generation = BundleGeneration(102);
            assert_eq!(keys.install_inner(&overlap), Ok(BundleGeneration(102)));
            for admission in [CacheKeyState::Active, CacheKeyState::Prepared] {
                let mut resurrected = overlap.clone();
                resurrected.generation = BundleGeneration(103);
                let mut old = removed[0].clone();
                old.state = admission;
                if admission == CacheKeyState::Active {
                    resurrected.cache_keys.remove(0);
                }
                resurrected.cache_keys.push(old);
                assert_eq!(
                    keys.install_inner(&resurrected),
                    Err(Error::InvalidConfiguration)
                );
            }
            assert_eq!(admitted_count(&keys), 2);
        }

        /// Both replay and rotation must validate every key's encoded generation.
        #[test]
        fn every_install_rejects_opaque_zero_and_future_key_generations() {
            let keys = keys();
            let roots = (*keys.peer_trust_roots().unwrap()).clone();
            for generation in [1, 2] {
                for id in [
                    KeyId([1; 16]),
                    KeyId(*b"RKG1\0\0\0\0\0\0\0\0\0\0\0\0"),
                    KeyId::from_generation(3, 1).unwrap(),
                ] {
                    let mut bad = bundle(generation, roots.clone(), CacheKeyState::Active);
                    bad.cache_keys[0].key.id = id;
                    assert_eq!(keys.install_inner(&bad), Err(Error::InvalidConfiguration));
                    assert_eq!(keys.generation().unwrap(), Some(1));
                }
            }
        }

        /// Public wire installation retains state and error mapping on rejection.
        #[test]
        fn public_install_preserves_generation_errors_and_installed_state() {
            let keys = keys();
            let roots = (*keys.peer_trust_roots().unwrap()).clone();
            for id in [
                KeyId([1; 16]),
                KeyId(*b"RKG1\0\0\0\0\0\0\0\0\0\0\0\0"),
                KeyId::from_generation(3, 0).unwrap(),
            ] {
                let bundle = racer_wire::control::KeyringBundle {
                    schema_version: racer_wire::control::KEYRING_SCHEMA_VERSION,
                    cluster: ClusterId(CLUSTER.into()),
                    generation: BundleGeneration(2),
                    peer_trust_roots: roots.clone(),
                    cache_keys: vec![racer_wire::control::CacheEncryptionKey::new(
                        CacheKeyRef {
                            cache: CacheId(CACHE.into()),
                            id,
                            purpose: CacheKeyPurpose::Page,
                        },
                        CacheKeyState::Active,
                        Zeroizing::new([9; 32]),
                    )],
                };
                assert_eq!(keys.install(bundle), Err(Error::InvalidConfiguration));
                assert_eq!(keys.generation().unwrap(), Some(1));
            }
        }

        /// Build a default pair with generation-one key IDs and configurable admission.
        fn bundle(generation: u64, roots: Vec<Vec<u8>>, state: CacheKeyState) -> KeyringBundle {
            KeyringBundle {
                schema_version: racer_wire::control::KEYRING_SCHEMA_VERSION,
                cluster: ClusterId(CLUSTER.into()),
                generation: BundleGeneration(generation),
                peer_trust_roots: roots,
                cache_keys: vec![
                    CacheEncryptionKey {
                        key: CacheKeyRef {
                            cache: CacheId(CACHE.into()),
                            id: KeyId::from_generation(1, 1).unwrap(),
                            purpose: CacheKeyPurpose::Page,
                        },
                        state,
                        material: Zeroizing::new([7; 32]),
                    },
                    CacheEncryptionKey {
                        key: CacheKeyRef {
                            cache: CacheId(CACHE.into()),
                            id: KeyId::from_generation(1, 2).unwrap(),
                            purpose: CacheKeyPurpose::OriginCredentials,
                        },
                        state,
                        material: Zeroizing::new([8; 32]),
                    },
                ],
            }
        }

        /// Rotation preserves prepared secret ownership and rejects invalid rebinding.
        #[test]
        fn rotation_rejects_rollback_rebinding_and_cross_purpose_use() {
            let keys = keys();
            let roots = (*keys.peer_trust_roots().unwrap()).clone();
            let cache = CacheId(CACHE.into());
            let lease = keys.active(&cache, KeyPurpose::Page).unwrap();
            assert!(lease.material(KeyPurpose::OriginCredentials).is_err());
            assert!(
                keys.install_inner(&bundle(1, roots.clone(), CacheKeyState::Active))
                    .is_ok()
            );
            let mut bad = bundle(2, roots.clone(), CacheKeyState::Active);
            bad.cache_keys[0].material = Zeroizing::new([9; 32]);
            assert!(keys.install_inner(&bad).is_err());
            let mut conflict = bundle(1, roots.clone(), CacheKeyState::Active);
            conflict.cache_keys[0].material = Zeroizing::new([9; 32]);
            assert!(keys.install_inner(&conflict).is_err());
            assert!(
                keys.install_inner(&bundle(0, roots.clone(), CacheKeyState::Active))
                    .is_err()
            );
            assert!(
                keys.install_inner(&bundle(2, roots.clone(), CacheKeyState::Prepared))
                    .is_err()
            );
            let mut rotated = bundle(2, roots.clone(), CacheKeyState::Active);
            rotated.cache_keys.push(CacheEncryptionKey {
                key: CacheKeyRef {
                    cache: cache.clone(),
                    id: KeyId::from_generation(2, 3).unwrap(),
                    purpose: CacheKeyPurpose::Page,
                },
                state: CacheKeyState::Prepared,
                material: Zeroizing::new([9; 32]),
            });
            keys.install_inner(&rotated).unwrap();
            assert!(
                keys.install_inner(&bundle(1, roots, CacheKeyState::Active))
                    .is_err()
            );
            assert!(keys.active(&cache, KeyPurpose::Page).is_ok());
            assert!(
                keys.lease(
                    Some(&cache),
                    KeyId::from_generation(2, 3).unwrap(),
                    KeyPurpose::Page
                )
                .is_ok()
            );
            assert!(
                keys.lease(Some(&cache), lease.id(), KeyPurpose::Page)
                    .is_ok()
            );
            assert_eq!(lease.material(KeyPurpose::Page).unwrap(), &[7; 32]);
            assert_eq!(
                keys.active(&cache, KeyPurpose::Page).unwrap().id(),
                lease.id()
            );
            let prepared = keys
                .lease(
                    Some(&cache),
                    KeyId::from_generation(2, 3).unwrap(),
                    KeyPurpose::Page,
                )
                .unwrap();
            assert_eq!(prepared.material(KeyPurpose::Page).unwrap(), &[9; 32]);
            // Activation retains the prepared material and drops the replaced key.
            rotated.generation = BundleGeneration(3);
            rotated.cache_keys.remove(0);
            rotated.cache_keys[1].state = CacheKeyState::Active;
            keys.install_inner(&rotated).unwrap();
            let active = keys.active(&cache, KeyPurpose::Page).unwrap();
            assert_eq!(active.id(), prepared.id());
            assert!(Arc::ptr_eq(&active.entry.secret, &prepared.entry.secret));
            assert!(
                keys.lease(Some(&cache), lease.id(), KeyPurpose::Page)
                    .is_err()
            );
            assert_eq!(lease.material(KeyPurpose::Page).unwrap(), &[7; 32]);
        }

        /// Retiring an epoch closes admission but its final lease still owns its bytes.
        #[test]
        fn retirement_closes_admission_and_last_lease_owns_secret() {
            let keys = keys();
            let cache = CacheId(CACHE.into());
            let lease = keys.active(&cache, KeyPurpose::Page).unwrap();
            let reference = lease.reference().clone();
            let mut next = bundle(
                2,
                (*keys.peer_trust_roots().unwrap()).clone(),
                CacheKeyState::Active,
            );
            next.cache_keys[0].key.id = KeyId::from_generation(2, 4).unwrap();
            next.cache_keys[0].material = Zeroizing::new([10; 32]);
            keys.install_inner(&next).unwrap();
            assert_eq!(keys.generation().unwrap(), Some(2));
            assert!(
                keys.lease(Some(&cache), reference.id, KeyPurpose::Page)
                    .is_err()
            );
            assert_eq!(
                keys.active(&cache, KeyPurpose::Page).unwrap().id(),
                KeyId::from_generation(2, 4).unwrap()
            );
            let secret = Arc::downgrade(&lease.entry.secret);
            assert_eq!(lease.material(KeyPurpose::Page).unwrap(), &[7; 32]);
            assert_eq!(secret.strong_count(), 1);
            // Exact replay must acknowledge the new epoch without reviving the old one.
            keys.install_inner(&next).unwrap();
            assert!(
                keys.lease(Some(&cache), reference.id, KeyPurpose::Page)
                    .is_err()
            );
            next.generation = BundleGeneration(3);
            keys.install_inner(&next).unwrap();
            assert_eq!(keys.generation().unwrap(), Some(3));
            assert!(
                keys.lease(Some(&cache), lease.id(), KeyPurpose::Page)
                    .is_err()
            );
            assert_eq!(lease.material(KeyPurpose::Page).unwrap(), &[7; 32]);
            assert_eq!(secret.strong_count(), 1);
            drop(lease);
            assert!(secret.upgrade().is_none());
            // A removed epoch cannot be resurrected in a later publication.
            assert!(
                keys.install_inner(&bundle(
                    4,
                    (*keys.peer_trust_roots().unwrap()).clone(),
                    CacheKeyState::Active
                ))
                .is_err()
            );
            assert!(
                keys.lease(Some(&cache), reference.id, KeyPurpose::Page)
                    .is_err()
            );
        }

        /// Empty admission preserves held leases and still forbids resurrection.
        #[test]
        fn removal_of_all_keys_preserves_leases_and_rejects_resurrection() {
            let keys = keys();
            let cache = CacheId(CACHE.into());
            let held = keys.active(&cache, KeyPurpose::Page).unwrap();
            let roots = (*keys.peer_trust_roots().unwrap()).clone();
            let mut empty = bundle(2, roots.clone(), CacheKeyState::Active);
            empty.cache_keys.clear();
            keys.install_inner(&empty).unwrap();
            keys.install_inner(&empty).unwrap();
            assert_eq!(admitted_count(&keys), 0);
            assert!(keys.active(&cache, KeyPurpose::Page).is_err());
            assert!(
                keys.lease(Some(&cache), held.id(), KeyPurpose::Page)
                    .is_err()
            );
            assert_eq!(held.material(KeyPurpose::Page).unwrap(), &[7; 32]);
            let mut resurrected = rotation_bundle(3, roots.clone());
            let mut old = bundle(1, roots.clone(), CacheKeyState::Prepared).cache_keys;
            resurrected.cache_keys.append(&mut old);
            assert_eq!(
                keys.install_inner(&resurrected),
                Err(Error::InvalidConfiguration)
            );
            assert_eq!(keys.generation().unwrap(), Some(2));
            keys.install_inner(&rotation_bundle(3, roots)).unwrap();
            assert!(keys.active(&cache, KeyPurpose::Page).is_ok());
            assert_eq!(held.material(KeyPurpose::Page).unwrap(), &[7; 32]);
        }

        /// Decrypt keys are leased for reading but never seal or become active,
        /// and only page keys may be decrypt keys, up to the retention bound.
        #[test]
        fn decrypt_keys_open_but_never_seal_or_activate() {
            let keys = keys();
            let cache = CacheId(CACHE.into());
            let roots = (*keys.peer_trust_roots().unwrap()).clone();
            let mut next = bundle(2, roots.clone(), CacheKeyState::Active);
            next.cache_keys[0].key.id = KeyId::from_generation(2, 1).unwrap();
            next.cache_keys[0].material = Zeroizing::new([9; 32]);
            let mut old = bundle(1, roots.clone(), CacheKeyState::Decrypt).cache_keys;
            old.truncate(1);
            next.cache_keys.append(&mut old);
            keys.install_inner(&next).unwrap();
            let old_id = KeyId::from_generation(1, 1).unwrap();
            let active = keys.active(&cache, KeyPurpose::Page).unwrap();
            assert_ne!(active.id(), old_id);
            let decrypt = keys.lease(Some(&cache), old_id, KeyPurpose::Page).unwrap();
            let mut sealed = [0; 19];
            let nonce: [u8; 24] = hex_fixture(&"01".repeat(24));
            assert_eq!(
                decrypt.seal_page(&cache, &nonce, b"aad", b"abc", &mut sealed),
                Err(Error::MissingKey)
            );
            active
                .seal_page(&cache, &nonce, b"aad", b"abc", &mut sealed)
                .unwrap();
            // Origin credential keys keep no decrypt history.
            let mut origin = bundle(3, roots.clone(), CacheKeyState::Active);
            origin.cache_keys[0].key.id = KeyId::from_generation(2, 1).unwrap();
            origin.cache_keys[0].material = Zeroizing::new([9; 32]);
            origin.cache_keys.push(CacheEncryptionKey {
                key: CacheKeyRef {
                    cache: cache.clone(),
                    id: KeyId::from_generation(3, 9).unwrap(),
                    purpose: CacheKeyPurpose::OriginCredentials,
                },
                state: CacheKeyState::Decrypt,
                material: Zeroizing::new([10; 32]),
            });
            assert_eq!(
                keys.install_inner(&origin),
                Err(Error::InvalidConfiguration)
            );
            // At most MAX_RETAINED_PAGE_KEYS - 1 decrypt page keys.
            let mut many = bundle(3, roots, CacheKeyState::Active);
            many.cache_keys[0].key.id = KeyId::from_generation(2, 1).unwrap();
            many.cache_keys[0].material = Zeroizing::new([9; 32]);
            for i in 0..MAX_RETAINED_PAGE_KEYS as u32 {
                let mut material = Zeroizing::new([11; 32]);
                material[..4].copy_from_slice(&i.to_be_bytes());
                many.cache_keys.push(CacheEncryptionKey {
                    key: CacheKeyRef {
                        cache: cache.clone(),
                        id: KeyId::from_generation(3, 100 + i).unwrap(),
                        purpose: CacheKeyPurpose::Page,
                    },
                    state: CacheKeyState::Decrypt,
                    material,
                });
            }
            assert_eq!(keys.install_inner(&many), Err(Error::InvalidConfiguration));
            many.cache_keys.pop();
            keys.install_inner(&many).unwrap();
        }

        /// Capacity counts only current admission, not previously issued leases.
        #[test]
        fn capacity_is_bounded_by_current_bundle_not_outstanding_leases() {
            let keys = keys();
            let held = keys
                .active(&CacheId(CACHE.into()), KeyPurpose::Page)
                .unwrap();
            let roots = (*keys.peer_trust_roots().unwrap()).clone();
            let mut full = rotation_bundle(2, roots.clone());
            let template = full.cache_keys[0].clone();
            full.cache_keys = (0..MAX_BUNDLE_KEYS as u32)
                .map(|i| {
                    let mut key = template.clone();
                    key.key.cache = CacheId(format!("{i:08x}-1111-4111-8111-111111111111"));
                    key.key.id = KeyId::from_generation(2, i).unwrap();
                    key.material[8..12].copy_from_slice(&i.to_be_bytes());
                    key
                })
                .collect();
            keys.install_inner(&full).unwrap();
            assert_eq!(admitted_count(&keys), MAX_BUNDLE_KEYS);
            full.generation = BundleGeneration(3);
            full.cache_keys.push(template);
            assert_eq!(keys.install_inner(&full), Err(Error::InvalidConfiguration));
            assert_eq!(keys.generation().unwrap(), Some(2));
            assert_eq!(admitted_count(&keys), MAX_BUNDLE_KEYS);
            keys.install_inner(&rotation_bundle(3, roots)).unwrap();
            assert_eq!(admitted_count(&keys), 2);
            assert_eq!(held.material(KeyPurpose::Page).unwrap(), &[7; 32]);
        }

        /// New identity admission follows current roots without invalidating held Arcs.
        #[test]
        fn identity_installation_and_leases_follow_current_trust() {
            let (pending, chain, roots) = super::tests::issued();
            let identity = Arc::new(
                pending
                    .accept(
                        ClusterId(CLUSTER.into()),
                        NodeId(NODE.into()),
                        chain,
                        &roots,
                        &WallClock::host(),
                    )
                    .unwrap(),
            );
            let keys = Keyring::new(
                ClusterId(CLUSTER.into()),
                NodeId(NODE.into()),
                Arc::new(KeyEpochs::default()),
            );
            keys.install_inner(&bundle(1, roots, CacheKeyState::Active))
                .unwrap();
            keys.install_signing_identity(identity.clone()).unwrap();
            let leased = keys.signing_identity().unwrap();
            assert_eq!(
                leased.sign(b"admitted operation").unwrap(),
                identity.sign(b"admitted operation").unwrap()
            );
            let (_, _, replacement_roots) = super::tests::issued();
            keys.install_inner(&bundle(2, replacement_roots, CacheKeyState::Active))
                .unwrap();
            assert!(keys.signing_identity().is_err());
            assert!(keys.install_signing_identity(identity).is_err());
            // Already admitted owners remain memory-safe during trust replacement.
            assert!(leased.sign(b"admitted operation").is_ok());
        }
    }

    /// Shared issuance fixtures for private state-machine tests.
    #[cfg(test)]
    mod tests {
        use super::*;

        /// Canonical cluster shared by private fixtures.
        pub(crate) const CLUSTER: &str = "11111111-1111-4111-8111-111111111111";

        /// Canonical node shared by private fixtures.
        pub(crate) const NODE: &str = "22222222-2222-4222-8222-222222222222";

        /// Canonical cache shared by private fixtures.
        pub(crate) const CACHE: &str = "33333333-3333-4333-8333-333333333333";

        /// Issue the default node certificate and its root.
        pub(super) fn issued() -> (PendingIdentity, Vec<Vec<u8>>, Vec<Vec<u8>>) {
            let (ca, ca_key) = test_util::ca();
            let (pending, chain) = test_util::issue(
                &ca,
                &ca_key,
                &ClusterId(CLUSTER.into()),
                &NodeId(NODE.into()),
                |_| {},
            );
            (pending, chain, vec![ca.der().to_vec()])
        }
    }
}
