//! Checks the C decrypt core (`native/gcm_core.h`, run serially through the
//! same chunked GHASH and CRC decomposition the CUDA kernels use) against the
//! `racer-crypto` XAES-256-GCM implementation and CRC-64/XZ.

use racer_gpu::gcm::{self, VERDICT_CRC, VERDICT_PASS, VERDICT_TAG};

/// Deterministic xorshift bytes.
struct Rng(u64);

impl Rng {
    /// Fill `out` with pseudo-random bytes.
    fn fill(&mut self, out: &mut [u8]) {
        for chunk in out.chunks_mut(8) {
            self.0 ^= self.0 << 13;
            self.0 ^= self.0 >> 7;
            self.0 ^= self.0 << 17;
            chunk.copy_from_slice(&self.0.to_le_bytes()[..chunk.len()]);
        }
    }

    /// A random array.
    fn array<const N: usize>(&mut self) -> [u8; N] {
        let mut out = [0; N];
        self.fill(&mut out);
        out
    }
}

/// A page sealed the way the dataplane seals pages.
struct Page {
    key: [u8; 32],
    nonce: [u8; 24],
    aad: Vec<u8>,
    plaintext: Vec<u8>,
    sealed: Vec<u8>,
}

/// Seal `length` random bytes with XAES-256-GCM and a page-shaped AAD.
fn page(rng: &mut Rng, length: usize) -> Page {
    let key = rng.array();
    let nonce = rng.array();
    // Same shape as security::page_aad: domain, cipher, length-prefixed
    // fields, page number, key ID, nonce and both lengths.
    let mut aad = b"racer/page/aead/v1\0".to_vec();
    aad.extend_from_slice(&1u32.to_be_bytes());
    aad.extend_from_slice(&36u32.to_be_bytes());
    aad.extend_from_slice(b"33333333-3333-4333-8333-333333333333");
    aad.extend_from_slice(&rng.array::<32>());
    aad.extend_from_slice(&4u32.to_be_bytes());
    aad.extend_from_slice(b"\"v1\"");
    aad.extend_from_slice(&7u64.to_be_bytes());
    aad.extend_from_slice(&rng.array::<16>());
    aad.extend_from_slice(&nonce);
    aad.extend_from_slice(&(length as u32).to_be_bytes());
    aad.extend_from_slice(&(length as u32 + 16).to_be_bytes());
    let mut plaintext = vec![0; length];
    rng.fill(&mut plaintext);
    let mut sealed = vec![0; length + racer_crypto::TAG_LEN];
    racer_crypto::seal(&key, &nonce, &aad, &plaintext, &mut sealed).unwrap();
    Page {
        key,
        nonce,
        aad,
        plaintext,
        sealed,
    }
}

/// Decrypt through the C core with the Rust-derived XAES key.
fn open(page: &Page, aad: &[u8], sealed: &[u8], crc: Option<u64>) -> (u32, u64, Vec<u8>) {
    let derived = racer_crypto::xaes_derive(&page.key, &page.nonce);
    let mut out = vec![0xa5; sealed.len() - 16];
    let verdict = gcm::decrypt(derived.key(), derived.nonce(), aad, sealed, crc, &mut out).unwrap();
    (verdict.status, verdict.crc, out)
}

#[test]
/// Lengths around block and chunk edges, including empty and 16 MiB pages.
fn core_matches_crypto_crate_for_many_lengths() {
    let mut rng = Rng(0x1234_5678_9abc_def1);
    let chunk = gcm::CHUNK as usize;
    let mut lengths = vec![
        0,
        1,
        15,
        16,
        17,
        31,
        33,
        255,
        4079,
        4080,
        4081,
        chunk - 1,
        chunk,
        chunk + 1,
        3 * chunk - 17,
        65536 + 5,
        1 << 20,
        (1 << 20) + 13,
        16 << 20,
    ];
    for _ in 0..8 {
        let mut b = [0u8; 4];
        rng.fill(&mut b);
        lengths.push(u32::from_le_bytes(b) as usize % 200_000);
    }
    for length in lengths {
        let page = page(&mut rng, length);
        let expected_crc = racer_crypto::crc64(&page.sealed);
        assert_eq!(gcm::crc64(&page.sealed), expected_crc, "length {length}");
        let (status, crc, out) = open(&page, &page.aad, &page.sealed, Some(expected_crc));
        assert_eq!(status, VERDICT_PASS, "length {length}");
        assert_eq!(crc, expected_crc, "length {length}");
        assert!(out == page.plaintext, "length {length}");
        let (status, crc, _) = open(&page, &page.aad, &page.sealed, None);
        assert_eq!(
            (status, crc),
            (VERDICT_PASS, expected_crc),
            "length {length}"
        );
    }
}

#[test]
/// Any tamper fails verification and leaves the output untouched.
fn core_rejects_tampering_without_writing() {
    let mut rng = Rng(42);
    for length in [1, 16, 4097, 70_001] {
        let page = page(&mut rng, length);
        let crc = racer_crypto::crc64(&page.sealed);
        let untouched = vec![0xa5; length];

        // Flipped ciphertext bit, with and without a matching stored CRC.
        let mut sealed = page.sealed.clone();
        sealed[length / 2] ^= 0x10;
        let (status, actual, out) = open(&page, &page.aad, &sealed, Some(crc));
        assert_eq!(status, VERDICT_CRC);
        assert_eq!(actual, racer_crypto::crc64(&sealed));
        assert_eq!(out, untouched);
        let fresh = racer_crypto::crc64(&sealed);
        let (status, _, out) = open(&page, &page.aad, &sealed, Some(fresh));
        assert_eq!(status, VERDICT_TAG);
        assert_eq!(out, untouched);

        // Flipped tag bit.
        let mut sealed = page.sealed.clone();
        sealed[length + 15] ^= 1;
        let (status, _, out) = open(&page, &page.aad, &sealed, None);
        assert_eq!(status, VERDICT_TAG);
        assert_eq!(out, untouched);

        // Wrong AAD.
        let mut aad = page.aad.clone();
        aad[20] ^= 1;
        let (status, _, out) = open(&page, &aad, &page.sealed, Some(crc));
        assert_eq!(status, VERDICT_TAG);
        assert_eq!(out, untouched);

        // Truncated AAD.
        let (status, _, _) = open(&page, &page.aad[..page.aad.len() - 1], &page.sealed, None);
        assert_eq!(status, VERDICT_TAG);

        // Wrong expected CRC with intact data.
        let (status, actual, out) = open(&page, &page.aad, &page.sealed, Some(crc ^ 1));
        assert_eq!((status, actual), (VERDICT_CRC, crc));
        assert_eq!(out, untouched);

        // Wrong key.
        let mut other = page.key;
        other[0] ^= 1;
        let derived = racer_crypto::xaes_derive(&other, &page.nonce);
        let mut out = untouched.clone();
        let verdict = gcm::decrypt(
            derived.key(),
            derived.nonce(),
            &page.aad,
            &page.sealed,
            None,
            &mut out,
        )
        .unwrap();
        assert_eq!(verdict.status, VERDICT_TAG);
        assert_eq!(out, untouched);
    }
}
