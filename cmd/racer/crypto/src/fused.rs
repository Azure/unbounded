//! Fused copy-and-open of one sealed page in cache-sized chunks.
//!
//! The receive path used to touch every byte of a registered RDMA page in two
//! separate full-page passes: a streaming copy into the ciphertext store, then
//! an out-of-place AES-256-GCM open from that copy into the destination. At
//! 16 MiB per page each pass streams through DRAM. Here each chunk of the
//! registered buffer is copied into the store and then decrypted while it is
//! still in the L2 cache, so the source is read from DRAM once.
//!
//! The streaming AES-256-GCM comes from the aws-lc `EVP_aes_256_gcm` cipher
//! (the same library and assembly that back [`crate::DerivedPageKey::open_into`]),
//! reached through the `aws-lc-sys` FFI that `aws-lc-rs` already links.

// The only unsafe code is the aws-lc EVP FFI below.
#![allow(unsafe_code)]

use crate::{Error, TAG_LEN};
use aws_lc_sys as sys;
use zeroize::Zeroize;

/// Default chunk size for [`crate::DerivedPageKey::open_copy_into`]: small
/// enough that a chunk read by the copy is still in L2 when it is decrypted.
pub const FUSED_CHUNK: usize = 128 << 10;

/// Where [`crate::DerivedPageKey::open_copy_into`] writes plaintext.
pub enum FusedOutput<'a> {
    /// Decrypt each chunk straight into `out`, which must cover the whole
    /// plaintext. Uses ordinary (temporal) stores.
    Whole(&'a mut [u8]),

    /// Decrypt each chunk into an L2-resident scratch buffer and pass the
    /// part overlapping plaintext bytes `start..start + out.len()` to the
    /// caller's copy function, which can use non-temporal stores and so skip
    /// the read for ownership of `out`. Also serves partial-page reads.
    Scratch {
        /// Destination for the requested plaintext bytes.
        out: &'a mut [u8],

        /// Plaintext offset of `out[0]`.
        start: usize,
    },
}

/// One aws-lc AES-256-GCM decryption context, freed (and its key schedule
/// cleansed by `OPENSSL_free`) on drop.
struct GcmDecrypt(*mut sys::EVP_CIPHER_CTX);

impl GcmDecrypt {
    fn new(key: &[u8; 32], nonce: &[u8; 12], aad: &[u8]) -> Result<Self, Error> {
        // SAFETY: allocation has no preconditions; null is checked.
        let ctx = unsafe { sys::EVP_CIPHER_CTX_new() };
        if ctx.is_null() {
            return Err(Error(()));
        }
        let this = Self(ctx);
        // SAFETY: `ctx` is live; the cipher is static; key and nonce are 32
        // and 12 bytes, matching AES-256-GCM's key and default IV length.
        let ok = unsafe {
            sys::EVP_DecryptInit_ex(
                ctx,
                sys::EVP_aes_256_gcm(),
                std::ptr::null_mut(),
                key.as_ptr(),
                nonce.as_ptr(),
            )
        };
        if ok != 1 {
            return Err(Error(()));
        }
        if !aad.is_empty() {
            let length = i32::try_from(aad.len()).map_err(|_| Error(()))?;
            let mut written = 0;
            // SAFETY: a null output pointer feeds `aad` as additional data.
            let ok = unsafe {
                sys::EVP_DecryptUpdate(
                    ctx,
                    std::ptr::null_mut(),
                    &mut written,
                    aad.as_ptr(),
                    length,
                )
            };
            if ok != 1 {
                return Err(Error(()));
            }
        }
        Ok(this)
    }

    /// Decrypt `input` into `out` (equal lengths, any alignment or length).
    fn update(&mut self, input: &[u8], out: &mut [u8]) -> Result<(), Error> {
        if input.len() != out.len() {
            return Err(Error(()));
        }
        if input.is_empty() {
            return Ok(());
        }
        let length = i32::try_from(input.len()).map_err(|_| Error(()))?;
        let mut written = 0;
        // SAFETY: equal-length live slices; GCM is a stream mode so the
        // output never exceeds the input.
        let ok = unsafe {
            sys::EVP_DecryptUpdate(
                self.0,
                out.as_mut_ptr(),
                &mut written,
                input.as_ptr(),
                length,
            )
        };
        if ok != 1 || written != length {
            return Err(Error(()));
        }
        Ok(())
    }

    /// Check the tag. Constant time inside aws-lc.
    fn finish(self, tag: &[u8]) -> Result<(), Error> {
        let mut tag: [u8; TAG_LEN] = tag.try_into().map_err(|_| Error(()))?;
        // SAFETY: `tag` is 16 writable bytes for the duration of the call.
        let ok = unsafe {
            sys::EVP_CIPHER_CTX_ctrl(
                self.0,
                sys::EVP_CTRL_GCM_SET_TAG,
                TAG_LEN as i32,
                tag.as_mut_ptr().cast(),
            )
        };
        if ok != 1 {
            return Err(Error(()));
        }
        let mut written = 0;
        let mut sink = [0u8; 16];
        // SAFETY: GCM final writes no bytes; `sink` covers a block anyway.
        let ok = unsafe { sys::EVP_DecryptFinal_ex(self.0, sink.as_mut_ptr(), &mut written) };
        if ok == 1 { Ok(()) } else { Err(Error(())) }
    }
}

impl Drop for GcmDecrypt {
    fn drop(&mut self) {
        // SAFETY: owned live context, freed once.
        unsafe { sys::EVP_CIPHER_CTX_free(self.0) };
    }
}

/// See [`crate::DerivedPageKey::open_copy_into`].
#[allow(clippy::too_many_arguments)]
pub(crate) fn open_copy_into(
    key: &[u8; 32],
    nonce: &[u8; 12],
    aad: &[u8],
    sealed: &[u8],
    store: &mut [u8],
    output: FusedOutput<'_>,
    chunk: usize,
    copy: &mut dyn FnMut(&mut [u8], &[u8]),
) -> Result<(), Error> {
    let length = sealed.len().checked_sub(TAG_LEN).ok_or(Error(()))?;
    if store.len() != sealed.len() || chunk == 0 {
        return Err(Error(()));
    }
    let (body, tag) = sealed.split_at(length);
    match output {
        FusedOutput::Whole(out) => {
            if out.len() != length {
                out.fill(0);
                return Err(Error(()));
            }
            let result = (|| {
                let mut gcm = GcmDecrypt::new(key, nonce, aad)?;
                let mut offset = 0;
                while offset < length {
                    let end = (offset + chunk).min(length);
                    // Copy first: it pulls the chunk into cache for the decrypt.
                    copy(&mut store[offset..end], &body[offset..end]);
                    gcm.update(&body[offset..end], &mut out[offset..end])?;
                    offset = end;
                }
                copy(&mut store[length..], tag);
                gcm.finish(tag)
            })();
            if result.is_err() {
                out.fill(0);
            }
            result
        }
        FusedOutput::Scratch { out, start } => {
            let Some(want_end) = start.checked_add(out.len()).filter(|end| *end <= length) else {
                out.fill(0);
                return Err(Error(()));
            };
            let mut scratch = zeroize::Zeroizing::new(vec![0u8; chunk.min(length)]);
            let result = (|| {
                let mut gcm = GcmDecrypt::new(key, nonce, aad)?;
                let mut offset = 0;
                while offset < length {
                    let end = (offset + chunk).min(length);
                    copy(&mut store[offset..end], &body[offset..end]);
                    let plain = &mut scratch[..end - offset];
                    gcm.update(&body[offset..end], plain)?;
                    let lo = offset.max(start);
                    let hi = end.min(want_end);
                    if lo < hi {
                        copy(
                            &mut out[lo - start..hi - start],
                            &plain[lo - offset..hi - offset],
                        );
                    }
                    offset = end;
                }
                copy(&mut store[length..], tag);
                gcm.finish(tag)
            })();
            scratch.zeroize();
            if result.is_err() {
                out.fill(0);
            }
            result
        }
    }
}
