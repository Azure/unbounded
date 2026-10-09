//! Public-API workflows, standard vectors, rejection checks, and CRC reference tests.

use racer_crypto::{SigningKey, TAG_LEN, VerifyingKey, crc64, ct_eq, hmac_sha256, open, seal};

/// The identity module keeps its public types and classified errors beside primitives.
#[test]
fn identity_module_public_api_preserves_key_and_error_contracts() {
    use racer_control_wire::{CacheId, ClusterId, NodeId};
    use racer_crypto::identity::{
        Certificates, Error as IdentityError, KeyEpochs, KeyLease, KeyPurpose, Keyring,
        PendingIdentity, Result as IdentityResult, SigningIdentity, VerifiedPeer, canonical_uuid,
        unix_time,
    };
    use std::{rc::Rc, sync::Arc};

    let cluster = ClusterId("11111111-1111-4111-8111-111111111111".into());
    let node = NodeId("22222222-2222-4222-8222-222222222222".into());
    let cache = CacheId("33333333-3333-4333-8333-333333333333".into());
    assert!(canonical_uuid(&cluster.0));
    let keys = Rc::new(Keyring::new(
        cluster.clone(),
        node.clone(),
        Arc::new(KeyEpochs::default()),
    ));
    let lease: IdentityResult<KeyLease> = keys.active(&cache, KeyPurpose::Page);
    assert!(matches!(lease, Err(IdentityError::MissingKey)));
    let identity: IdentityResult<Arc<SigningIdentity>> = keys.signing_identity();
    assert!(matches!(identity, Err(IdentityError::MissingKey)));
    let certificates = Certificates::new(cluster, keys);
    let peer: IdentityResult<VerifiedPeer> =
        certificates.verify_signed(&[], &node, b"message", &[]);
    assert!(matches!(peer, Err(IdentityError::MissingKey)));
    let _time = unix_time();

    let key = SigningKey::from_seed(&[42; 32]);
    let encoded = key.to_pkcs8_der().unwrap();
    let pending = PendingIdentity::recover(&encoded).unwrap();
    assert_eq!(*pending.export_pkcs8_for_persistence().unwrap(), *encoded);
    assert!(!pending.csr_der().unwrap().is_empty());
    assert!(matches!(
        PendingIdentity::recover(&[0; 4097]),
        Err(IdentityError::InvalidRequest)
    ));
    assert!(matches!(
        PendingIdentity::recover(b"invalid"),
        Err(IdentityError::Unauthorized)
    ));
    let primitive: Result<SigningKey, racer_crypto::Error> = SigningKey::from_pkcs8_der(b"invalid");
    assert!(primitive.is_err());
}

/// Exchange records in reused slices while preserving surrounding and rejected output.
#[test]
fn exchange_records_with_reusable_caller_buffers() {
    let key = random_material();
    let mut send = [0xa5; 128];
    let mut receive = [0x5a; 128];

    // Each record has a distinct caller-supplied nonce. Empty records still
    // authenticate their context. Only the selected output slices may change.
    for (sequence, plaintext) in [b"first record".as_slice(), b"", b"next record"]
        .into_iter()
        .enumerate()
    {
        let mut nonce = random_material();
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
            let key = random_material();
            let nonce = random_material();
            let plaintext: Vec<u8> = (0..length).map(|i| i as u8).collect();
            let mut sealed = vec![0xa5; length + TAG_LEN];
            seal(&key, &nonce, aad, &plaintext, &mut sealed).unwrap();
            let retained = sealed.clone();
            let mut opened = vec![0xa5; length];
            open(&key, &nonce, aad, &sealed, &mut opened).unwrap();
            assert_eq!(opened, plaintext);
            assert_eq!(sealed, retained);
            // Empty plaintext still requires a valid authentication tag.
            sealed[length] ^= 1;
            opened.fill(0xa5);
            assert!(open(&key, &nonce, aad, &sealed, &mut opened).is_err());
            assert_eq!(opened, vec![0xa5; length]);
        }
    }
}

/// Reject changes to ciphertext, tag, key, nonce, or AAD without touching output.
#[test]
fn aead_tampering_never_writes_output() {
    let plaintext = b"immutable input and output on authentication failure";
    let key = random_material();
    let nonce = random_material();
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
    let key = random_material();
    let nonce = random_material();
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

/// Generate key or nonce bytes for tests that do not require published vectors.
fn random_material<const N: usize>() -> [u8; N] {
    ring::rand::generate(&ring::rand::SystemRandom::new())
        .unwrap()
        .expose()
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

/// Identity workflows exercise the same public boundary used by the dataplane.
mod identity_workflows {
    use super::random_material;
    use racer_control_wire::{
        BundleGeneration, CacheEncryptionKey, CacheId, CacheKeyRef, CacheKeyState, ClusterId,
        KeyId, KeyringBundle, NodeId, SCHEMA_VERSION,
    };
    use racer_crypto::identity::{
        Certificates, Error, KeyEpochs, KeyPurpose, Keyring, PendingIdentity, SigningIdentity,
        unix_time,
    };
    use rustls::pki_types::{CertificateDer, PrivateKeyDer, PrivatePkcs8KeyDer};
    use std::{
        rc::Rc,
        sync::Arc,
        time::{Duration, UNIX_EPOCH},
    };
    use zeroize::Zeroizing;

    /// Canonical cluster for certificate and keyring workflows.
    const CLUSTER: &str = "11111111-1111-4111-8111-111111111111";

    /// Canonical node authenticated by the test certificates.
    const NODE: &str = "22222222-2222-4222-8222-222222222222";

    /// Canonical cache authorized by the test key epochs.
    const CACHE: &str = "33333333-3333-4333-8333-333333333333";

    /// TLS time follows the current scope and clamps/truncates epoch offsets.
    #[test]
    fn tls_time_uses_current_scoped_wall_clock_clamps_and_truncates() {
        let clock = uring_runtime::environment::SimulationClock::new(71);
        let _environment = clock.environment(0).enter();
        for (wall, seconds) in [
            (UNIX_EPOCH - Duration::from_nanos(1), 0),
            (UNIX_EPOCH, 0),
            (UNIX_EPOCH + Duration::from_millis(1999), 1),
            (UNIX_EPOCH + Duration::from_secs(10), 10),
        ] {
            clock.set_wall_time(wall);
            assert_eq!(unix_time().as_secs(), seconds);
        }
    }

    /// Issuing for an existing key keeps its bytes and caller-specified dates.
    #[cfg(feature = "test-util")]
    #[test]
    fn existing_key_and_customized_certificate_contract_are_preserved() {
        use racer_crypto::identity::test_util::{ca, issue_pending};

        let (ca, ca_key) = ca();
        let cluster = ClusterId(CLUSTER.into());
        let node = NodeId(NODE.into());
        let pending = PendingIdentity::generate().unwrap();
        let original = pending.export_pkcs8_for_persistence().unwrap();
        let (pending, chain) = issue_pending(pending, &ca, &ca_key, &cluster, &node, |params| {
            params.not_before = rcgen::date_time_ymd(2020, 1, 1);
            params.not_after = rcgen::date_time_ymd(2030, 1, 1);
        });
        assert_eq!(*pending.export_pkcs8_for_persistence().unwrap(), *original);
        let (_, cert) = x509_parser::parse_x509_certificate(&chain[0]).unwrap();
        assert_eq!(cert.validity().not_before.timestamp(), 1_577_836_800);
        assert_eq!(cert.validity().not_after.timestamp(), 1_893_456_000);
        pending
            .accept(cluster, node, chain, &[ca.der().to_vec()])
            .unwrap();
    }

    /// Recovery, CSR generation, and TLS pairing use the same accepted key.
    #[test]
    fn identity_recovery_csr_tls_and_key_pairing() {
        let (pending, chain, roots) = issued_with(|_| {});
        assert!(!pending.csr_der().unwrap().is_empty());
        let bytes = pending.export_pkcs8_for_persistence().unwrap();
        let identity = pending
            .accept(
                ClusterId(CLUSTER.into()),
                NodeId(NODE.into()),
                chain.clone(),
                &roots,
            )
            .unwrap();
        let private = PrivateKeyDer::Pkcs8(PrivatePkcs8KeyDer::from(bytes.as_slice()));
        let tls_key = rustls::crypto::ring::sign::any_supported_type(&private).unwrap();
        let certified = rustls::sign::CertifiedKey::new(
            identity
                .certificate_chain()
                .iter()
                .cloned()
                .map(CertificateDer::from)
                .collect(),
            tls_key,
        );
        assert!(certified.keys_match().is_ok());
        let recovered = SigningIdentity::from_pkcs8(
            identity.cluster().clone(),
            identity.node().clone(),
            &bytes,
            chain.clone(),
            &roots,
        )
        .unwrap();
        assert_eq!(
            identity.sign(b"exact message").unwrap(),
            recovered.sign(b"exact message").unwrap()
        );
        assert!(
            PendingIdentity::generate()
                .unwrap()
                .accept(
                    identity.cluster().clone(),
                    identity.node().clone(),
                    chain.clone(),
                    &roots
                )
                .is_err()
        );
        assert!(
            SigningIdentity::from_pkcs8(
                identity.cluster().clone(),
                NodeId("other".into()),
                &bytes,
                chain,
                &roots
            )
            .is_err()
        );
    }

    /// Verify canonical node chains and reject malformed or altered signatures.
    #[test]
    fn validates_chain_node_and_strict_signature() {
        let (pending, chain, roots) = issued_with(|_| {});
        let cluster = ClusterId(CLUSTER.into());
        let node = NodeId(NODE.into());
        let secret = pending.export_pkcs8_for_persistence().unwrap();
        for (cluster, node) in [
            (ClusterId("other".into()), node.clone()),
            (cluster.clone(), NodeId("other".into())),
        ] {
            assert!(
                PendingIdentity::recover(&secret)
                    .unwrap()
                    .accept(cluster, node, chain.clone(), &roots)
                    .is_err()
            );
        }
        let identity = pending
            .accept(cluster.clone(), node.clone(), chain.clone(), &roots)
            .unwrap();
        let keys = Rc::new(keyring(roots));
        let certificates = Certificates::new(cluster.clone(), keys.clone());
        let signature = identity.sign(b"message").unwrap();
        assert!(
            certificates
                .verify_signed(&chain, &node, b"message", &signature)
                .is_ok()
        );
        assert!(
            certificates
                .verify_signed(&chain, &node, b"changed", &signature)
                .is_err()
        );
        assert!(
            certificates
                .verify_signed(&chain, &node, b"message", &signature[..63])
                .is_err()
        );
        let mut oversized = signature.clone();
        oversized.push(0);
        assert!(
            certificates
                .verify_signed(&chain, &node, b"message", &oversized)
                .is_err()
        );
        assert!(
            certificates
                .verify_signed(&chain, &NodeId("other".into()), b"message", &signature)
                .is_err()
        );
        let foreign = Certificates::new(ClusterId("other".into()), keys);
        assert!(
            foreign
                .verify_signed(&chain, &node, b"message", &signature)
                .is_err()
        );
        let (_, _, foreign_roots) = issued_with(|_| {});
        let foreign = Certificates::new(cluster, Rc::new(keyring(foreign_roots)));
        assert!(
            foreign
                .verify_signed(&chain, &node, b"message", &signature)
                .is_err()
        );
        let mut bad = chain.clone();
        bad[0].push(0);
        assert!(
            certificates
                .verify_signed(&bad, &node, b"message", &signature)
                .is_err()
        );
        assert!(
            certificates
                .verify_signed(&vec![chain[0].clone(); 9], &node, b"message", &signature)
                .is_err()
        );
    }

    /// Acceptance, recovery, and peer verification require exact client usages.
    #[test]
    fn identity_paths_require_exact_client_certificate_usages() {
        use rcgen::{ExtendedKeyUsagePurpose as Eku, KeyUsagePurpose as Ku};

        for (case, extra_ku, extra_eku) in [
            ("exact", None, None),
            ("key-encipherment", Some(Ku::KeyEncipherment), None),
            ("server-auth", None, Some(Eku::ServerAuth)),
            ("unknown", None, Some(Eku::Other(vec![1, 2, 3, 4]))),
            ("any", None, Some(Eku::Any)),
            ("code-signing", None, Some(Eku::CodeSigning)),
            ("email-protection", None, Some(Eku::EmailProtection)),
            ("time-stamping", None, Some(Eku::TimeStamping)),
            ("ocsp-signing", None, Some(Eku::OcspSigning)),
        ] {
            let (pending, chain, roots) = issued_with(|params| {
                params.key_usages.extend(extra_ku);
                params.extended_key_usages.extend(extra_eku);
            });
            let cluster = ClusterId(CLUSTER.into());
            let node = NodeId(NODE.into());
            let secret = pending.export_pkcs8_for_persistence().unwrap();
            let signature = racer_crypto::SigningKey::from_pkcs8_der(&secret)
                .unwrap()
                .sign(b"message");
            let expected = if case == "exact" {
                Ok(())
            } else {
                Err(Error::Unauthorized)
            };
            assert_eq!(
                pending
                    .accept(cluster.clone(), node.clone(), chain.clone(), &roots)
                    .map(|_| ()),
                expected,
                "accept: {case}"
            );
            assert_eq!(
                SigningIdentity::from_pkcs8(
                    cluster.clone(),
                    node.clone(),
                    &secret,
                    chain.clone(),
                    &roots,
                )
                .map(|_| ()),
                expected,
                "recover: {case}"
            );
            let certificates = Certificates::new(cluster, Rc::new(keyring(roots)));
            assert_eq!(
                certificates
                    .verify_signed(&chain, &node, b"message", &signature)
                    .map(|_| ()),
                expected,
                "peer: {case}"
            );
        }
    }

    /// Exercise usage, validity, CA-leaf, and ambiguous URI rejection.
    #[test]
    fn rejects_missing_usage_ca_expiration_and_ambiguous_identity() {
        for case in 0..8 {
            let (pending, chain, roots) = issued_with(|params| match case {
                0 => params.key_usages.clear(),
                1 => params.key_usages = vec![rcgen::KeyUsagePurpose::KeyEncipherment],
                2 => params.extended_key_usages.clear(),
                3 => params.extended_key_usages = vec![rcgen::ExtendedKeyUsagePurpose::ServerAuth],
                4 => params.is_ca = rcgen::IsCa::Ca(rcgen::BasicConstraints::Unconstrained),
                5 => {
                    params.not_before = rcgen::date_time_ymd(2000, 1, 1);
                    params.not_after = rcgen::date_time_ymd(2001, 1, 1);
                }
                6 => params
                    .subject_alt_names
                    .push(params.subject_alt_names[0].clone()),
                _ => {
                    params.subject_alt_names = vec![rcgen::SanType::URI(
                        format!("spiffe://{CLUSTER}/node/{NODE}/extra")
                            .try_into()
                            .unwrap(),
                    )]
                }
            });
            assert!(
                pending
                    .accept(
                        ClusterId(CLUSTER.into()),
                        NodeId(NODE.into()),
                        chain,
                        &roots
                    )
                    .is_err(),
                "case {case}"
            );
        }
    }

    /// Page and credential operations enforce bindings without touching failed output.
    #[test]
    fn borrowed_outputs_enforce_purpose_cache_id_and_retained_epoch() {
        let (_, _, roots) = issued_with(|_| {});
        let keys = keyring(roots.clone());
        let cache = CacheId(CACHE.into());
        let page = keys.active(&cache, KeyPurpose::Page).unwrap();
        let credentials = keys.active(&cache, KeyPurpose::OriginCredentials).unwrap();
        let page_nonce = random_material();
        let mut sealed = [0; 19];
        page.seal_page(&cache, &page_nonce, b"aad", b"abc", &mut sealed)
            .unwrap();
        let mut out = [42; 3];
        page.open_page(&cache, page.id(), &page_nonce, b"aad", &sealed, &mut out)
            .unwrap();
        assert_eq!(&out, b"abc");
        out.fill(42);
        assert_eq!(
            page.open_page(
                &cache,
                credentials.id(),
                &page_nonce,
                b"aad",
                &sealed,
                &mut out
            ),
            Err(Error::MissingKey)
        );
        assert_eq!(out, [42; 3]);
        assert_eq!(
            page.open_page(
                &CacheId("wrong".into()),
                page.id(),
                &page_nonce,
                b"aad",
                &sealed,
                &mut out
            ),
            Err(Error::MissingKey)
        );
        assert_eq!(out, [42; 3]);
        assert_eq!(
            page.open_page(&cache, page.id(), &page_nonce, b"bad", &sealed, &mut out),
            Err(Error::CorruptRecord)
        );
        assert_eq!(out, [42; 3]);
        let original = sealed;
        assert_eq!(
            credentials.seal_page(&cache, &page_nonce, b"aad", b"abc", &mut sealed),
            Err(Error::MissingKey)
        );
        assert_eq!(sealed, original);
        assert_eq!(
            page.seal_credentials(&cache, &page_nonce, b"aad", b"abc", &mut sealed),
            Err(Error::MissingKey)
        );
        assert_eq!(sealed, original);
        let credential_nonce = random_material();
        credentials
            .seal_credentials(&cache, &credential_nonce, b"aad", b"abc", &mut sealed)
            .unwrap();
        credentials
            .open_credentials(
                &cache,
                credentials.id(),
                &credential_nonce,
                b"aad",
                &sealed,
                &mut out,
            )
            .unwrap();
        assert_eq!(&out, b"abc");
        assert_eq!(
            credentials.open_credentials(
                &cache,
                credentials.id(),
                &credential_nonce,
                b"bad",
                &sealed,
                &mut out
            ),
            Err(Error::Unauthorized)
        );
        assert_eq!(&out, b"abc");
        keys.install(bundle(2, roots)).unwrap();
        assert!(
            keys.lease(Some(&cache), page.id(), KeyPurpose::Page)
                .is_err()
        );
        let retained_page_nonce = random_material();
        page.seal_page(&cache, &retained_page_nonce, b"aad", b"abc", &mut sealed)
            .unwrap();
        page.open_page(
            &cache,
            page.id(),
            &retained_page_nonce,
            b"aad",
            &sealed,
            &mut out,
        )
        .unwrap();
        assert_eq!(&out, b"abc");
    }

    /// Request MAC derivation is domain-separated and enforces the credential epoch.
    #[test]
    fn request_mac_derives_internally_and_rejects_wrong_purpose_or_message() {
        let (_, _, roots) = issued_with(|_| {});
        let keys = keyring(roots);
        let cache = CacheId(CACHE.into());
        let page = keys.active(&cache, KeyPurpose::Page).unwrap();
        let key = keys.active(&cache, KeyPurpose::OriginCredentials).unwrap();
        let mut tag = [0; 32];
        assert_eq!(
            page.request_mac(&cache, b"message", &mut tag),
            Err(Error::MissingKey)
        );
        assert_eq!(tag, [0; 32]);
        key.request_mac(&cache, b"message", &mut tag).unwrap();
        let mut domain = b"racer/request-mac/key/v1\0".to_vec();
        domain.extend_from_slice(&(cache.0.len() as u32).to_be_bytes());
        domain.extend_from_slice(cache.0.as_bytes());
        domain.extend_from_slice(&key.id().0);
        let derived = Zeroizing::new(racer_crypto::hmac_sha256(&[8; 32], &domain));
        assert_eq!(tag, racer_crypto::hmac_sha256(&derived, b"message"));
        key.verify_request_mac(&cache, key.id(), b"message", &tag)
            .unwrap();
        assert_eq!(
            key.verify_request_mac(&cache, key.id(), b"changed", &tag),
            Err(Error::Unauthorized)
        );
        assert_eq!(
            key.verify_request_mac(&cache, page.id(), b"message", &tag),
            Err(Error::MissingKey)
        );
        assert_eq!(
            key.verify_request_mac(&cache, key.id(), b"message", &tag[..31]),
            Err(Error::Unauthorized)
        );
    }

    /// Shuffled groups and prepared keys use the same ordering for all admission paths.
    #[test]
    fn shuffled_epoch_groups_preserve_exact_and_active_admission() {
        let (_, _, roots) = issued_with(|_| {});
        let keys = keyring(roots.clone());
        let mut next = bundle(2, roots);
        let mut records = Vec::new();
        for (cache_index, cache) in [CACHE, CLUSTER, NODE].into_iter().enumerate() {
            for (purpose_index, purpose) in [KeyPurpose::Page, KeyPurpose::OriginCredentials]
                .into_iter()
                .enumerate()
            {
                for prepared in [false, true] {
                    let ordinal =
                        (cache_index * 4 + purpose_index * 2 + usize::from(prepared)) as u32;
                    records.push(CacheEncryptionKey::new(
                        CacheKeyRef {
                            cache: CacheId(cache.into()),
                            id: KeyId::from_generation(2, ordinal).unwrap(),
                            purpose,
                        },
                        if prepared {
                            CacheKeyState::Prepared
                        } else {
                            CacheKeyState::Active
                        },
                        Zeroizing::new([ordinal as u8 + 32; 32]),
                    ));
                }
            }
        }
        records.reverse();
        next.cache_keys = records;
        let expected: Vec<_> = next
            .cache_keys
            .iter()
            .map(|record| (record.key.clone(), record.state))
            .collect();
        keys.install(next).unwrap();
        for (reference, state) in expected {
            let leased = keys
                .lease(Some(&reference.cache), reference.id, reference.purpose)
                .unwrap();
            assert_eq!(leased.reference(), &reference);
            let active = keys.active(&reference.cache, reference.purpose).unwrap();
            if state == CacheKeyState::Active {
                assert_eq!(active.reference(), &reference);
            } else {
                assert_ne!(active.id(), reference.id);
            }
        }
        assert!(
            keys.lease(
                None,
                KeyId::from_generation(2, 0).unwrap(),
                KeyPurpose::Page
            )
            .is_err()
        );
        assert!(
            keys.active(
                &CacheId("44444444-4444-4444-8444-444444444444".into()),
                KeyPurpose::Page
            )
            .is_err()
        );
    }

    /// Accepted identities and cached peer keys share conservative validity boundaries.
    #[test]
    fn accepted_chain_validity_is_enforced_at_expiry_and_after_clock_rollback() {
        let clock = uring_runtime::environment::SimulationClock::new(97);
        let _environment = clock.environment(0).enter();
        clock.set_wall_time(UNIX_EPOCH + Duration::from_secs(1_700_000_000));
        let (pending, chain, roots) = issued_with(|params| {
            params.not_before = rcgen::date_time_ymd(2020, 1, 1);
            params.not_after = rcgen::date_time_ymd(2030, 1, 1);
        });
        let node = NodeId(NODE.into());
        let cluster = ClusterId(CLUSTER.into());
        let identity = Arc::new(
            pending
                .accept(cluster.clone(), node.clone(), chain.clone(), &roots)
                .unwrap(),
        );
        assert_eq!(identity.expires_at_seconds(), 1_893_456_000);
        let keys = Rc::new(keyring(roots));
        keys.install_signing_identity(identity.clone()).unwrap();
        let certificates = Certificates::new(cluster, keys.clone());
        let signature = identity.sign(b"validity").unwrap();
        certificates
            .verify_signed(&chain, &node, b"validity", &signature)
            .unwrap();
        clock.set_wall_time(UNIX_EPOCH + Duration::from_secs(1_893_455_999));
        assert!(keys.signing_identity().is_ok());
        assert!(identity.sign(b"validity").is_ok());
        clock.set_wall_time(UNIX_EPOCH + Duration::from_secs(1_893_456_000));
        assert!(identity.sign(b"validity").is_err());
        clock.set_wall_time(UNIX_EPOCH + Duration::from_secs(1_893_456_001));
        assert!(keys.signing_identity().is_err());
        assert!(identity.sign(b"validity").is_err());
        assert!(
            certificates
                .verify_signed(&chain, &node, b"validity", &signature)
                .is_err()
        );
        clock.set_wall_time(UNIX_EPOCH + Duration::from_secs(1_500_000_000));
        assert!(keys.signing_identity().is_err());
        assert!(identity.sign(b"validity").is_err());
        assert!(
            certificates
                .verify_signed(&chain, &node, b"validity", &signature)
                .is_err()
        );
    }

    /// Install one active key per purpose using only the public bundle interface.
    fn keyring(roots: Vec<Vec<u8>>) -> Keyring {
        let keys = Keyring::new(
            ClusterId(CLUSTER.into()),
            NodeId(NODE.into()),
            Arc::new(KeyEpochs::default()),
        );
        keys.install(bundle(1, roots)).unwrap();
        keys
    }

    /// Construct fresh generation-bound keys with distinct purpose-specific material.
    fn bundle(generation: u64, roots: Vec<Vec<u8>>) -> KeyringBundle {
        KeyringBundle {
            schema_version: SCHEMA_VERSION,
            cluster: ClusterId(CLUSTER.into()),
            generation: BundleGeneration(generation),
            peer_trust_roots: roots,
            cache_keys: [KeyPurpose::Page, KeyPurpose::OriginCredentials]
                .into_iter()
                .enumerate()
                .map(|(ordinal, purpose)| {
                    CacheEncryptionKey::new(
                        CacheKeyRef {
                            cache: CacheId(CACHE.into()),
                            id: KeyId::from_generation(generation, ordinal as u32).unwrap(),
                            purpose,
                        },
                        CacheKeyState::Active,
                        Zeroizing::new([generation as u8 * 2 + 5 + ordinal as u8; 32]),
                    )
                })
                .collect(),
        }
    }

    /// Issue a customizable certificate without enabling production fixture APIs.
    fn issued_with(
        customize: impl FnOnce(&mut rcgen::CertificateParams),
    ) -> (PendingIdentity, Vec<Vec<u8>>, Vec<Vec<u8>>) {
        let mut ca_params = rcgen::CertificateParams::new(Vec::<String>::new()).unwrap();
        ca_params.is_ca = rcgen::IsCa::Ca(rcgen::BasicConstraints::Unconstrained);
        ca_params.key_usages = vec![
            rcgen::KeyUsagePurpose::KeyCertSign,
            rcgen::KeyUsagePurpose::CrlSign,
        ];
        let ca_key = rcgen::KeyPair::generate_for(&rcgen::PKCS_ED25519).unwrap();
        let ca = ca_params.self_signed(&ca_key).unwrap();
        let pending = PendingIdentity::generate().unwrap();
        let secret = pending.export_pkcs8_for_persistence().unwrap();
        let key = rcgen::KeyPair::from_pkcs8_der_and_sign_algo(
            &PrivatePkcs8KeyDer::from(secret.as_slice()),
            &rcgen::PKCS_ED25519,
        )
        .unwrap();
        let mut params = rcgen::CertificateParams::new(Vec::<String>::new()).unwrap();
        params.subject_alt_names = vec![rcgen::SanType::URI(
            format!("spiffe://{CLUSTER}/node/{NODE}")
                .try_into()
                .unwrap(),
        )];
        params.key_usages = vec![rcgen::KeyUsagePurpose::DigitalSignature];
        params.extended_key_usages = vec![rcgen::ExtendedKeyUsagePurpose::ClientAuth];
        customize(&mut params);
        let cert = params.signed_by(&key, &ca, &ca_key).unwrap();
        (pending, vec![cert.der().to_vec()], vec![ca.der().to_vec()])
    }
}
