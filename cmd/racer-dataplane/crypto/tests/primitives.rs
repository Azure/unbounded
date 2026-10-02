use racer_crypto::{
    aead::{self, KEY_LEN, NONCE_LEN, TAG_LEN},
    ct_eq,
    ed25519::{SigningKey, VerifyingKey},
    hmac_sha256,
};

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

fn array<const N: usize>(bytes: &str) -> [u8; N] {
    hex(bytes).try_into().unwrap()
}

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
    assert_eq!((KEY_LEN, NONCE_LEN, TAG_LEN), (32, 24, 16));
    let mut sealed = vec![0xa5; plaintext.len() + TAG_LEN];
    aead::seal(&key, &nonce, &aad, plaintext, &mut sealed).unwrap();
    assert_eq!(sealed, expected);
    let mut opened = vec![0xa5; plaintext.len()];
    aead::open(&key, &nonce, &aad, &expected, &mut opened).unwrap();
    assert_eq!(opened, plaintext);
}

#[test]
fn aead_empty_and_block_boundaries() {
    for length in [0, 1, 15, 16, 17, 63, 64, 65, 255, 256, 257, 4096] {
        for aad in [b"".as_slice(), b"associated data"] {
            let plaintext: Vec<u8> = (0..length).map(|i| i as u8).collect();
            let mut sealed = vec![0xa5; length + TAG_LEN];
            aead::seal(&[7; 32], &[2; 24], aad, &plaintext, &mut sealed).unwrap();
            let retained = sealed.clone();
            let mut opened = vec![0xa5; length];
            aead::open(&[7; 32], &[2; 24], aad, &sealed, &mut opened).unwrap();
            assert_eq!(opened, plaintext);
            assert_eq!(sealed, retained);
            // Empty plaintext still requires a valid authentication tag.
            sealed[length] ^= 1;
            opened.fill(0xa5);
            assert!(aead::open(&[7; 32], &[2; 24], aad, &sealed, &mut opened).is_err());
            assert_eq!(opened, vec![0xa5; length]);
        }
    }
}

#[test]
fn aead_tampering_never_writes_output() {
    let plaintext = b"immutable input and output on authentication failure";
    let key = [7; 32];
    let nonce = [2; 24];
    let aad = b"associated data";
    let mut sealed = vec![0; plaintext.len() + TAG_LEN];
    aead::seal(&key, &nonce, aad, plaintext, &mut sealed).unwrap();
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
        assert!(aead::open(&key, &nonce, &aad, &input, &mut output).is_err());
        assert_eq!(output, vec![0xa5; plaintext.len()], "fault={fault}");
        assert_eq!(input, retained, "fault={fault}");
    }
}

#[test]
fn aead_malformed_lengths_never_panic_or_write_output() {
    let key = [7; 32];
    let nonce = [2; 24];
    let error = aead::open(&key, &nonce, b"", b"", &mut []).unwrap_err();
    let error: &dyn std::error::Error = &error;
    assert_eq!(error.to_string(), "cryptographic operation failed");
    assert!(error.source().is_none());
    let plaintext = b"hello";
    let mut sealed = vec![0; plaintext.len() + TAG_LEN];
    aead::seal(&key, &nonce, b"", plaintext, &mut sealed).unwrap();
    for length in [0, 1, 4, 6, 16, 20, 21, 22, 64] {
        let mut output = vec![0xa5; length];
        assert!(aead::open(&key, &nonce, b"", &sealed, &mut output).is_err());
        assert_eq!(output, vec![0xa5; length]);
    }
    for length in 0..TAG_LEN {
        for out_len in [0, 1, 5, 16] {
            let mut output = vec![0xa5; out_len];
            assert!(aead::open(&key, &nonce, b"", &sealed[..length], &mut output).is_err());
            assert_eq!(output, vec![0xa5; out_len]);
        }
    }
    for length in [0, 1, 5, 16, 20, 22, 64] {
        let mut output = vec![0xa5; length];
        assert!(aead::seal(&key, &nonce, b"", plaintext, &mut output).is_err());
        assert_eq!(output, vec![0xa5; length]);
    }
}

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
