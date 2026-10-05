//! Public-API workflows, standard vectors, rejection checks, and CRC reference tests.

use racer_crypto::{SigningKey, TAG_LEN, VerifyingKey, crc64, ct_eq, hmac_sha256, open, seal};

/// Exchange records in reused slices while preserving surrounding and rejected output.
#[test]
fn exchange_records_with_reusable_caller_buffers() {
    let key = [7; 32];
    let mut send = [0xa5; 128];
    let mut receive = [0x5a; 128];

    // Each record has a distinct caller-supplied nonce. Empty records still
    // authenticate their context. Only the selected output slices may change.
    for (sequence, plaintext) in [b"first record".as_slice(), b"", b"next record"]
        .into_iter()
        .enumerate()
    {
        let mut nonce = [0; 24];
        nonce[..8].copy_from_slice(&(sequence as u64).to_le_bytes());
        let context = b"example/records/v1";
        let sealed_end = 3 + plaintext.len() + TAG_LEN;
        let opened_end = 5 + plaintext.len();
        let previous_send = send;
        let previous_receive = receive;

        seal(&key, &nonce, context, plaintext, &mut send[3..sealed_end]).unwrap();
        assert_eq!(&send[..3], &previous_send[..3]);
        assert_eq!(&send[sealed_end..], &previous_send[sealed_end..]);

        // A rejected record must not destroy an earlier result in reused output.
        assert!(
            open(
                &key,
                &nonce,
                b"example/other-context/v1",
                &send[3..sealed_end],
                &mut receive[5..opened_end],
            )
            .is_err()
        );
        assert_eq!(receive, previous_receive);

        // Retry the original record into that same buffer without resetting it.
        let transmitted = send;
        open(
            &key,
            &nonce,
            context,
            &send[3..sealed_end],
            &mut receive[5..opened_end],
        )
        .unwrap();
        assert_eq!(&receive[5..opened_end], plaintext);
        assert_eq!(&receive[..5], &previous_receive[..5]);
        assert_eq!(&receive[opened_end..], &previous_receive[opened_end..]);
        assert_eq!(send, transmitted);
    }
}

/// Authenticate shared-key messages and reject modified or truncated transmissions.
#[test]
fn authenticate_messages_with_a_shared_key() {
    let sender_key = [0x37; 32];
    let receiver_key = sender_key;
    let long_message = [0x81; 129];
    for message in [b"hello".as_slice(), b"", long_message.as_slice()] {
        let transmitted_tag = hmac_sha256(&sender_key, message);
        let expected = hmac_sha256(&receiver_key, message);
        assert!(ct_eq(&transmitted_tag, &expected));

        let mut changed_message = message.to_vec();
        changed_message.push(0);
        assert!(!ct_eq(
            &transmitted_tag,
            &hmac_sha256(&receiver_key, &changed_message)
        ));
        assert!(!ct_eq(&transmitted_tag, &hmac_sha256(&[0x38; 32], message)));
        assert!(!ct_eq(&transmitted_tag[..31], &expected));
        let mut changed_tag = transmitted_tag;
        changed_tag[31] ^= 1;
        assert!(!ct_eq(&changed_tag, &expected));

        // A failed comparison does not consume the key or the expected tag.
        assert!(ct_eq(
            &transmitted_tag,
            &hmac_sha256(&receiver_key, message)
        ));
    }
}

/// Match the published XChaCha20-Poly1305 ciphertext and authentication tag.
#[test]
fn xchacha_draft_known_answer() {
    // draft-irtf-cfrg-xchacha-03, Appendix A.1 (AEAD_XCHACHA20_POLY1305).
    // https://www.ietf.org/archive/id/draft-irtf-cfrg-xchacha-03.txt
    let key = array("808182838485868788898a8b8c8d8e8f909192939495969798999a9b9c9d9e9f");
    let nonce = array("404142434445464748494a4b4c4d4e4f5051525354555657");
    let aad = hex("50515253c0c1c2c3c4c5c6c7");
    let plaintext = b"Ladies and Gentlemen of the class of '99: \
        If I could offer you only one tip for the future, sunscreen would be it.";
    let expected = hex(concat!(
        "bd6d179d3e83d43b9576579493c0e939572a1700252bfaccbed2902c21396cbb",
        "731c7f1b0b4aa6440bf3a82f4eda7e39ae64c6708c54c216cb96b72e1213",
        "b4522f8c9ba40db5d945b11b69b982c1bb9e3f3fac2bc369488f76b2383565",
        "d3fff921f9664c97637da9768812f615c68b13b52e",
        "c0875924c1c7987947deafd8780acf49"
    ));
    assert_eq!((key.len(), nonce.len(), TAG_LEN), (32, 24, 16));
    let mut sealed = vec![0xa5; plaintext.len() + TAG_LEN];
    seal(&key, &nonce, &aad, plaintext, &mut sealed).unwrap();
    assert_eq!(sealed, expected);
    let mut opened = vec![0xa5; plaintext.len()];
    open(&key, &nonce, &aad, &expected, &mut opened).unwrap();
    assert_eq!(opened, plaintext);
}

/// Round-trip empty and boundary-sized messages and reject their modified tags.
#[test]
fn aead_empty_and_block_boundaries() {
    for length in [0, 1, 15, 16, 17, 63, 64, 65, 255, 256, 257, 4096] {
        for aad in [b"".as_slice(), b"associated data"] {
            let plaintext: Vec<u8> = (0..length).map(|i| i as u8).collect();
            let mut sealed = vec![0xa5; length + TAG_LEN];
            seal(&[7; 32], &[2; 24], aad, &plaintext, &mut sealed).unwrap();
            let retained = sealed.clone();
            let mut opened = vec![0xa5; length];
            open(&[7; 32], &[2; 24], aad, &sealed, &mut opened).unwrap();
            assert_eq!(opened, plaintext);
            assert_eq!(sealed, retained);
            // Empty plaintext still requires a valid authentication tag.
            sealed[length] ^= 1;
            opened.fill(0xa5);
            assert!(open(&[7; 32], &[2; 24], aad, &sealed, &mut opened).is_err());
            assert_eq!(opened, vec![0xa5; length]);
        }
    }
}

/// Reject changes to ciphertext, tag, key, nonce, or AAD without touching output.
#[test]
fn aead_tampering_never_writes_output() {
    let plaintext = b"immutable input and output on authentication failure";
    let key = [7; 32];
    let nonce = [2; 24];
    let aad = b"associated data";
    let mut sealed = vec![0; plaintext.len() + TAG_LEN];
    seal(&key, &nonce, aad, plaintext, &mut sealed).unwrap();
    // Every byte of ciphertext and tag, then key, nonce, and AAD.
    for fault in 0..sealed.len() + 3 {
        let mut key = key;
        let mut nonce = nonce;
        let mut aad = *aad;
        let mut input = sealed.clone();
        match fault {
            f if f < sealed.len() => input[f] ^= 1,
            f if f == sealed.len() => key[0] ^= 1,
            f if f == sealed.len() + 1 => nonce[0] ^= 1,
            _ => aad[0] ^= 1,
        }
        let retained = input.clone();
        let mut output = vec![0xa5; plaintext.len()];
        assert!(open(&key, &nonce, &aad, &input, &mut output).is_err());
        assert_eq!(output, vec![0xa5; plaintext.len()], "fault={fault}");
        assert_eq!(input, retained, "fault={fault}");
    }
}

/// Reject incorrect buffer lengths with an opaque error and unchanged output.
#[test]
fn aead_malformed_lengths_never_panic_or_write_output() {
    let key = [7; 32];
    let nonce = [2; 24];
    let error = open(&key, &nonce, b"", b"", &mut []).unwrap_err();
    let error: &dyn std::error::Error = &error;
    assert_eq!(error.to_string(), "cryptographic operation failed");
    assert!(error.source().is_none());
    let plaintext = b"hello";
    let mut sealed = vec![0; plaintext.len() + TAG_LEN];
    seal(&key, &nonce, b"", plaintext, &mut sealed).unwrap();
    for length in [0, 1, 4, 6, 16, 20, 21, 22, 64] {
        let mut output = vec![0xa5; length];
        assert!(open(&key, &nonce, b"", &sealed, &mut output).is_err());
        assert_eq!(output, vec![0xa5; length]);
    }
    for length in 0..TAG_LEN {
        for out_len in [0, 1, 5, 16] {
            let mut output = vec![0xa5; out_len];
            assert!(open(&key, &nonce, b"", &sealed[..length], &mut output).is_err());
            assert_eq!(output, vec![0xa5; out_len]);
        }
    }
    for length in [0, 1, 5, 16, 20, 22, 64] {
        let mut output = vec![0xa5; length];
        assert!(seal(&key, &nonce, b"", plaintext, &mut output).is_err());
        assert_eq!(output, vec![0xa5; length]);
    }
}

/// Match the first two RFC 4231 HMAC-SHA256 vectors with padded fixed-size keys.
#[test]
fn rfc4231_hmac_cases_1_and_2() {
    // https://www.rfc-editor.org/rfc/rfc4231.html#section-4.2 and section 4.3.
    // Zero-padding these short RFC keys to 32 bytes preserves HMAC semantics.
    let mut key = [0; 32];
    key[..20].fill(0x0b);
    assert_eq!(
        hmac_sha256(&key, b"Hi There"),
        array("b0344c61d8db38535ca8afceaf0bf12b881dc200c9833da726e9376c2e32cff7")
    );
    let mut key = [0; 32];
    key[..4].copy_from_slice(b"Jefe");
    assert_eq!(
        hmac_sha256(&key, b"what do ya want for nothing?"),
        array("5bdcc146bf60754e6a042426089575c75a003f089d2739839dec58b964ec3843")
    );
}

/// Compare empty slices, unequal lengths, and a change at every byte position.
#[test]
fn equality_handles_empty_unequal_lengths_and_each_differing_byte() {
    assert!(ct_eq(b"", b""));
    assert!(!ct_eq(b"", b"a"));
    assert!(!ct_eq(b"a", b""));
    for length in [1, 16, 32, 64, 257] {
        let bytes = vec![0x5a; length];
        assert!(ct_eq(&bytes, &bytes));
        assert!(!ct_eq(&bytes, &bytes[..length - 1]));
        assert!(!ct_eq(&bytes[..length - 1], &bytes));
        for offset in 0..length {
            let mut changed = bytes.clone();
            changed[offset] ^= 1;
            assert!(!ct_eq(&bytes, &changed));
            assert!(!ct_eq(&changed, &bytes));
        }
    }
}

/// Match the first three RFC 8032 signatures and reject changed messages or signatures.
#[test]
fn rfc8032_ed25519_vectors_1_through_3() {
    // RFC 8032 section 7.1, TEST 1, TEST 2, TEST 3.
    // https://www.rfc-editor.org/rfc/rfc8032.html#section-7.1
    for (seed, public, message, signature) in [
        (
            "9d61b19deffd5a60ba844af492ec2cc44449c5697b326919703bac031cae7f60",
            "d75a980182b10ab7d54bfed3c964073a0ee172f3daa62325af021a68f707511a",
            "",
            concat!(
                "e5564300c360ac729086e2cc806e828a84877f1eb8e5d974d873e06522490155",
                "5fb8821590a33bacc61e39701cf9b46bd25bf5f0595bbe24655141438e7a100b"
            ),
        ),
        (
            "4ccd089b28ff96da9db6c346ec114e0f5b8a319f35aba624da8cf6ed4fb8a6fb",
            "3d4017c3e843895a92b70aa74d1b7ebc9c982ccf2ec4968cc0cd55f12af4660c",
            "72",
            concat!(
                "92a009a9f0d4cab8720e820b5f642540a2b27b5416503f8fb3762223ebdb69da",
                "085ac1e43e15996e458f3613d0f11d8c387b2eaeb4302aeeb00d291612bb0c00"
            ),
        ),
        (
            "c5aa8df43f9f837bedb7442f31dcb7b166d38535076f094b85ce3a2e0b4458f7",
            "fc51cd8e6218a1a38da47ed00230f0580816ed13ba3303ac5deb911548908025",
            "af82",
            concat!(
                "6291d657deec24024827e69c3abe01a30ce548a284743a445e3680d7db5ac3ac",
                "18ff9b538d16f290ae67f760984dc6594a7c15e9716ed28dc027beceea1ec40a"
            ),
        ),
    ] {
        let key = SigningKey::from_seed(&array(seed));
        let public = array(public);
        let verifying = VerifyingKey::from_bytes(&public).unwrap();
        assert_eq!(key.verifying_key(), verifying);
        assert_eq!(verifying.as_bytes(), &public);
        let message = hex(message);
        let signature: [u8; 64] = array(signature);
        assert_eq!(key.sign(&message), signature);
        verifying.verify_strict(&message, &signature).unwrap();
        let mut wrong_message = message.clone();
        wrong_message.push(0);
        assert!(verifying.verify_strict(&wrong_message, &signature).is_err());
        for offset in 0..signature.len() {
            let mut changed = signature;
            changed[offset] ^= 1;
            assert!(verifying.verify_strict(&message, &changed).is_err());
        }
    }
}

/// Reject malformed encodings, noncanonical scalars, weak points, and wrong keys.
#[test]
fn ed25519_strict_verification_rejects_malformed_and_weak_signatures() {
    let key = SigningKey::from_seed(&[7; 32]);
    let verifying = key.verifying_key();
    let signature = key.sign(b"message");
    for length in 0..64 {
        assert!(
            verifying
                .verify_strict(b"message", &signature[..length])
                .is_err()
        );
    }
    assert!(verifying.verify_strict(b"message", &[0; 65]).is_err());
    let mut noncanonical_scalar = signature;
    noncanonical_scalar[32..].fill(0xff);
    assert!(
        verifying
            .verify_strict(b"message", &noncanonical_scalar)
            .is_err()
    );
    let mut malformed_r = signature;
    malformed_r[..32].fill(2);
    assert!(verifying.verify_strict(b"message", &malformed_r).is_err());
    assert!(VerifyingKey::from_bytes(&[2; 32]).is_err());

    // Identity public key and R with S=0 satisfy the cofactored equation, but
    // strict verification must reject these small-order points.
    let mut identity = [0; 32];
    identity[0] = 1;
    let weak = VerifyingKey::from_bytes(&identity).unwrap();
    let mut forged = [0; 64];
    forged[..32].copy_from_slice(&identity);
    assert!(weak.verify_strict(b"any message", &forged).is_err());
    assert!(verifying.verify_strict(b"message", &forged).is_err());
    assert!(
        SigningKey::from_seed(&[8; 32])
            .verifying_key()
            .verify_strict(b"message", &signature)
            .is_err()
    );
}

/// Restore persisted signing keys and reject invalid or inconsistent PKCS#8 documents.
#[test]
fn ed25519_pkcs8_roundtrip_and_malformed_documents() {
    let key = SigningKey::from_seed(&[42; 32]);
    let der: zeroize::Zeroizing<Vec<u8>> = key.to_pkcs8_der().unwrap();
    let imported = SigningKey::from_pkcs8_der(&der).unwrap();
    assert_eq!(imported.verifying_key(), key.verifying_key());
    assert_eq!(imported.sign(b"roundtrip"), key.sign(b"roundtrip"));
    assert_eq!(*imported.to_pkcs8_der().unwrap(), *der);
    // A receiver needs only the public bytes; the original signer can be gone.
    let public_bytes = *key.verifying_key().as_bytes();
    drop(key);
    let receiver = VerifyingKey::from_bytes(&public_bytes).unwrap();
    let message = b"message signed after restoring the persisted key";
    let signature = imported.sign(message);
    receiver.verify_strict(message, &signature).unwrap();
    assert!(
        receiver
            .verify_strict(b"changed message", &signature)
            .is_err()
    );
    for length in 0..der.len() {
        assert!(SigningKey::from_pkcs8_der(&der[..length]).is_err());
    }
    assert!(SigningKey::from_pkcs8_der(b"not DER").is_err());
    let mut wrong_algorithm = der.clone();
    // id-Ed25519 = 1.3.101.112; replace it with id-X25519 = 1.3.101.110.
    let oid = wrong_algorithm
        .windows(3)
        .position(|w| w == [0x2b, 0x65, 0x70])
        .unwrap();
    wrong_algorithm[oid + 2] = 0x6e;
    assert!(SigningKey::from_pkcs8_der(&wrong_algorithm).is_err());
    let mut mismatched_public = der.clone();
    let public = imported.verifying_key();
    let public_offset = mismatched_public
        .windows(32)
        .position(|w| w == public.as_bytes())
        .unwrap();
    mismatched_public[public_offset..public_offset + 32]
        .copy_from_slice(SigningKey::from_seed(&[43; 32]).verifying_key().as_bytes());
    assert!(SigningKey::from_pkcs8_der(&mismatched_public).is_err());
    let mut trailing_bytes = der.clone();
    trailing_bytes.push(0);
    assert!(SigningKey::from_pkcs8_der(&trailing_bytes).is_err());
}

/// Compare CRC golden values and varied alignments and lengths with a bitwise oracle.
#[test]
fn xz_golden_and_hardware_equivalence() {
    for (input, expected) in [(b"".as_slice(), 0), (b"123456789", 0x995d_c9bb_df19_39fa)] {
        assert_eq!(reference(input), expected);
        assert_eq!(crc64(input), expected);
    }
    let bytes = random_bytes(65537 + 32);
    for offset in 0..32 {
        for length in [
            0, 1, 7, 8, 9, 15, 16, 17, 31, 32, 63, 64, 127, 128, 129, 143, 144, 255, 256, 257,
            1023, 1024, 1025, 4095, 4096, 4097, 65535, 65536, 65537,
        ] {
            assert_equivalent(&bytes[offset..offset + length]);
        }
    }
    for chunk in bytes.chunks_exact(3).take(128) {
        let length = usize::from(u16::from_le_bytes([chunk[0], chunk[1]]));
        let offset = usize::from(chunk[2] & 31);
        assert_equivalent(&bytes[offset..offset + length]);
    }
}

/// Check a full-size page plus tag against the oracle and incremental CRC updates.
#[test]
fn sixteen_mib_and_tag_match_independent_reference() {
    let length = 16 * 1024 * 1024 + 16;
    let bytes = random_bytes(length + 1);
    assert_equivalent(&bytes[1..]);
    let mut digest = crc64fast::Digest::new();
    for chunk in bytes[1..].chunks(4093) {
        digest.write(chunk);
    }
    assert_eq!(digest.sum64(), crc64(&bytes[1..]));
}

/// Exercise CRC dispatch on supported x86 hardware, failing on unsupported hosts.
#[test]
#[ignore = "requires x86 PCLMULQDQ, SSE2 and SSE4.1; run explicitly on supported hardware"]
fn pclmul_hardware_path_executes_when_available() {
    #[cfg(any(target_arch = "x86", target_arch = "x86_64"))]
    {
        assert!(std::is_x86_feature_detected!("pclmulqdq"));
        assert!(std::is_x86_feature_detected!("sse2"));
        assert!(std::is_x86_feature_detected!("sse4.1"));
        assert_equivalent(&random_bytes(16384));
        eprintln!("CRC64/XZ crc64fast dispatch executed: x86 PCLMULQDQ");
    }
    #[cfg(not(any(target_arch = "x86", target_arch = "x86_64")))]
    panic!("x86 hardware verification requires an x86 host");
}

/// Exercise CRC dispatch on supported AArch64 hardware, failing on unsupported hosts.
#[test]
#[ignore = "requires AArch64 PMULL and NEON; run explicitly on supported hardware"]
fn pmull_hardware_path_executes_when_available() {
    #[cfg(target_arch = "aarch64")]
    {
        assert!(std::arch::is_aarch64_feature_detected!("pmull"));
        assert!(std::arch::is_aarch64_feature_detected!("neon"));
        assert_equivalent(&random_bytes(16384));
        eprintln!("CRC64/XZ crc64fast dispatch executed: AArch64 PMULL");
    }
    #[cfg(not(target_arch = "aarch64"))]
    panic!("PMULL hardware verification requires an AArch64 host");
}

/// Decode an even-length hexadecimal test vector.
fn hex(bytes: &str) -> Vec<u8> {
    assert_eq!(bytes.len() % 2, 0);
    bytes
        .as_bytes()
        .chunks_exact(2)
        .map(|pair| {
            let digit = |byte: u8| (byte as char).to_digit(16).unwrap() as u8;
            digit(pair[0]) << 4 | digit(pair[1])
        })
        .collect()
}

/// Decode a hexadecimal vector and require the expected array length.
fn array<const N: usize>(bytes: &str) -> [u8; N] {
    hex(bytes).try_into().unwrap()
}

/// Compute an independent bitwise CRC without library tables or folding constants.
fn reference(bytes: &[u8]) -> u64 {
    let mut crc = u64::MAX;
    for byte in bytes {
        crc ^= u64::from(*byte);
        for _ in 0..8 {
            crc = (crc >> 1)
                ^ if crc & 1 != 0 {
                    0xc96c_5795_d787_0f42
                } else {
                    0
                };
        }
    }
    !crc
}

/// Generate reproducible nonuniform bytes for CRC length and alignment checks.
fn random_bytes(length: usize) -> Vec<u8> {
    let mut state = 0x6a09_e667_f3bc_c909u64;
    (0..length)
        .map(|_| {
            state ^= state << 13;
            state ^= state >> 7;
            state ^= state << 17;
            state as u8
        })
        .collect()
}

/// Require dispatched and table-based CRC implementations to match the oracle.
fn assert_equivalent(bytes: &[u8]) {
    let expected = reference(bytes);
    assert_eq!(crc64(bytes), expected, "dispatch, length={}", bytes.len());
    let mut table = crc64fast::Digest::new_table();
    table.write(bytes);
    assert_eq!(table.sum64(), expected, "table, length={}", bytes.len());
}
