//! Host emulation of the GPU decrypt kernels, from `native/gcm_host.c`.
//!
//! It runs the exact `native/gcm_core.h` steps the CUDA kernels run, serially.
//! The simulation backend uses it as its kernel, and the tests compare it with
//! `racer-crypto`.

use std::ffi::c_int;

/// Verification result. Matches `struct gcm_verdict` and
/// `struct racer_gpu_verdict` in C.
#[repr(C)]
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub struct RawVerdict {
    /// One of the `VERDICT_*` values.
    pub status: u32,

    /// Always zero.
    pub reserved: u32,

    /// CRC-64/XZ of the sealed page, computed by the kernel.
    pub crc: u64,
}

/// Verification did not run.
pub const VERDICT_NONE: u32 = 0;

/// CRC (if requested) and tag matched; the plaintext was written.
pub const VERDICT_PASS: u32 = 0x600d;

/// The CRC did not match. Nothing was written.
pub const VERDICT_CRC: u32 = 2;

/// The tag did not match. Nothing was written.
pub const VERDICT_TAG: u32 = 3;

/// Bytes covered by each parallel partial (`GCM_CHUNK`).
pub const CHUNK: u64 = 4096;

unsafe extern "C" {
    fn racer_gcm_host_crc64(data: *const u8, length: u64) -> u64;

    fn racer_gcm_host_aes256(key: *const [u8; 32], input: *const [u8; 16], out: *mut [u8; 16]);

    fn racer_gcm_host_decrypt(
        key: *const [u8; 32],
        nonce: *const [u8; 12],
        aad: *const u8,
        aad_len: u64,
        sealed: *const u8,
        n: u64,
        check_crc: u32,
        expected_crc: u64,
        out: *mut u8,
        verdict: *mut RawVerdict,
    ) -> c_int;
}

/// CRC-64/XZ through the chunked combine path the kernels use.
pub fn crc64(data: &[u8]) -> u64 {
    unsafe { racer_gcm_host_crc64(data.as_ptr(), data.len() as u64) }
}

/// One AES-256 block encryption.
pub fn aes256(key: &[u8; 32], block: &[u8; 16]) -> [u8; 16] {
    let mut out = [0; 16];
    unsafe { racer_gcm_host_aes256(key, block, &mut out) };
    out
}

/// Verify `sealed` (ciphertext then 16-byte tag) and decrypt it into `out`
/// with the derived AES-256-GCM key and nonce. `out` must be exactly the
/// ciphertext length and is written only when the verdict passes.
///
/// Returns `None` if the lengths do not match or scratch memory is unavailable.
pub fn decrypt(
    key: &[u8; 32],
    nonce: &[u8; 12],
    aad: &[u8],
    sealed: &[u8],
    expected_crc: Option<u64>,
    out: &mut [u8],
) -> Option<RawVerdict> {
    if sealed.len().checked_sub(16) != Some(out.len()) {
        return None;
    }
    let mut verdict = RawVerdict::default();
    let rc = unsafe {
        racer_gcm_host_decrypt(
            key,
            nonce,
            aad.as_ptr(),
            aad.len() as u64,
            sealed.as_ptr(),
            out.len() as u64,
            expected_crc.is_some() as u32,
            expected_crc.unwrap_or(0),
            out.as_mut_ptr(),
            &mut verdict,
        )
    };
    (rc == 0).then_some(verdict)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    /// FIPS-197 appendix C.3 AES-256 known answer.
    fn aes256_known_answer() {
        let key: [u8; 32] = std::array::from_fn(|i| i as u8);
        let block: [u8; 16] = std::array::from_fn(|i| (i as u8) * 0x11);
        assert_eq!(
            aes256(&key, &block),
            [
                0x8e, 0xa2, 0xb7, 0xca, 0x51, 0x67, 0x45, 0xbf, 0xea, 0xfc, 0x49, 0x90, 0x4b, 0x49,
                0x60, 0x89
            ]
        );
    }

    #[test]
    /// The CRC-64/XZ check value, and empty input.
    fn crc64_check_value() {
        assert_eq!(crc64(b"123456789"), 0x995d_c9bb_df19_39fa);
        assert_eq!(crc64(b""), 0);
    }

    #[test]
    /// NIST GCM test case 14: AES-256, zero key, one zero block, no AAD.
    fn gcm_nist_test_case_14() {
        let ciphertext = [
            0xce, 0xa7, 0x40, 0x3d, 0x4d, 0x60, 0x6b, 0x6e, 0x07, 0x4e, 0xc5, 0xd3, 0xba, 0xf3,
            0x9d, 0x18,
        ];
        let tag = [
            0xd0, 0xd1, 0xc8, 0xa7, 0x99, 0x99, 0x6b, 0xf0, 0x26, 0x5b, 0x98, 0xb5, 0xd4, 0x8a,
            0xb9, 0x19,
        ];
        let sealed = [ciphertext, tag].concat();
        let mut out = [1; 16];
        let verdict = decrypt(&[0; 32], &[0; 12], &[], &sealed, None, &mut out).unwrap();
        assert_eq!(verdict.status, VERDICT_PASS);
        assert_eq!(out, [0; 16]);
    }

    #[test]
    /// Mismatched output length is refused before any work.
    fn decrypt_rejects_length_mismatch() {
        let mut out = [0; 3];
        assert!(decrypt(&[0; 32], &[0; 12], &[], &[0; 20], None, &mut out).is_none());
        assert!(decrypt(&[0; 32], &[0; 12], &[], &[0; 2], None, &mut []).is_none());
    }
}
