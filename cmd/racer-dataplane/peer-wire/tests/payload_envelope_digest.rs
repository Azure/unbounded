use base64::{Engine, engine::general_purpose::STANDARD};
use http1::{Header, MessageHead, StartLine};
use peer_wire::{Error, ForwardedHead, MAX_ENVELOPE_HEAD, SignedHead, control::envelope_digest};
use std::sync::Arc;

const PARAMS: &str = "(\"@status\" \"content-length\" \"racer-signer\" \"racer-timestamp\");created=1700000000;keyid=\"00000000-1111-4111-8111-111111111111\";alg=\"ed25519\";tag=\"racer-peer-v5\"";

fn signed_head(index: u8) -> SignedHead {
    // Hashing binds these fixed signature bytes; it does not verify Ed25519.
    let signature = vec![0x11 * (index + 1); 64];
    SignedHead {
        head: MessageHead {
            start: StartLine::Response {
                status: 200 + u16::from(index),
            },
            headers: vec![
                Header::new("content-length", "0"),
                Header::new("racer-signer", "00000000-1111-4111-8111-111111111111"),
                Header::new("racer-timestamp", "1700000000123"),
                Header::new("signature-input", format!("racer={PARAMS}")),
                Header::new(
                    "signature",
                    format!("racer=:{}:", STANDARD.encode(&signature)),
                ),
            ],
        },
        signature,
    }
}

fn envelope(hops: &[u8]) -> ForwardedHead {
    ForwardedHead {
        original: Arc::new(signed_head(0)),
        hops: hops.iter().copied().map(signed_head).collect(),
    }
}

fn digest(hex: &str) -> [u8; 32] {
    assert_eq!(hex.len(), 64);
    std::array::from_fn(|i| u8::from_str_radix(&hex[2 * i..2 * i + 2], 16).unwrap())
}

// Vectors were computed with Python hashlib.sha256, without Rust digest helpers.
// Each base is the literal RFC 9421 response base for the fields above, ending
// with `"@signature-params": ` plus PARAMS, with no trailing newline.
// Inner input: b"racer-peer-v2/signed-head\0" + BE64(base length) + base
//              + BE64(64) + the 64 fixed signature bytes.
// Inner SHA-256 values for indexes 0, 1, 2:
// 0829b086e9c0a725046361b66c728cecbbd0af5112a707d7b61eeeaf2d485192
// 6df0a7c33e219cbdd3fbd588eb687c61f8ca61c9b97fb7ba77fb8a3adc2734e6
// 3bf1e560ba607f38fd7c923db04cb658ef22eed851781d0a1f50b9d91fc41ce0
// Outer input: b"racer-peer-v1/payload-envelope\0" + BE64(hop count)
//              + original inner digest + ordered hop inner digests.
// The v1 outer and v2 inner domains stay fixed even with the v5 signature tag.
#[test]
fn zero_hop_exact_vector_preserves_domain_compatibility() {
    assert_eq!(
        envelope_digest(&envelope(&[])),
        Ok(digest(
            "402dd1a513f5ad99df14b70f548d530133a99dfd0333e69f09ed00ed902a8d52"
        ))
    );
}

#[test]
fn exact_vectors_bind_hop_count_order_and_duplicates() {
    for (hops, expected) in [
        (
            &[1][..],
            "020b2ae1e6f219453468aefe5ee011361be56e75fc7e7011dca45163de5a3478",
        ),
        (
            &[1, 2][..],
            "729d41e96f72215414e98ac6bdd2f41d53a2aa1f5c0a716b17fe09ce92e99f4f",
        ),
        (
            &[2, 1][..],
            "6295b199d45fd267d615549e70b0f7adbd597811d82e70152b6c4abd3251c556",
        ),
        (
            &[1, 1][..],
            "cc59c8edec25400cc6b0d304576142131bcd39231d190e1c3b90d0ad221de86d",
        ),
        (
            &[1, 2, 1][..],
            "ba0d5f60fd839e21c5e2570e5c6fa70d101222f3feb83e1f3c87a29e6138c254",
        ),
    ] {
        assert_eq!(
            envelope_digest(&envelope(hops)),
            Ok(digest(expected)),
            "hops={hops:?}"
        );
    }
}

fn head_at(auth: &mut ForwardedHead, position: usize) -> &mut SignedHead {
    if position == 0 {
        Arc::get_mut(&mut auth.original).unwrap()
    } else {
        &mut auth.hops[position - 1]
    }
}

#[test]
fn digest_binds_original_and_every_hop_head_and_signature() {
    let expected = digest("729d41e96f72215414e98ac6bdd2f41d53a2aa1f5c0a716b17fe09ce92e99f4f");
    for position in 0..=2 {
        for change_signature in [false, true] {
            let mut auth = envelope(&[1, 2]);
            let signed = head_at(&mut auth, position);
            if change_signature {
                signed.signature[0] ^= 1;
                signed.head.headers.last_mut().unwrap().value =
                    format!("racer=:{}:", STANDARD.encode(&signed.signature)).into_bytes();
            } else {
                signed.head.start = StartLine::Response { status: 203 };
            }
            assert_ne!(
                envelope_digest(&auth).unwrap(),
                expected,
                "position={position} change_signature={change_signature}"
            );
        }
    }
}

#[test]
fn malformed_original_and_each_hop_preserve_signed_head_errors() {
    for hops in [&[][..], &[1, 2][..]] {
        for position in 0..=hops.len() {
            for expected in [
                Error::Unauthorized,
                Error::InvalidRequest,
                Error::HeaderTooLarge,
            ] {
                let mut auth = envelope(hops);
                let signed = head_at(&mut auth, position);
                match expected {
                    Error::Unauthorized => {
                        signed.head.headers[3].value = b"racer=invalid".to_vec();
                    }
                    Error::InvalidRequest => {
                        signed.head.headers[0].name = "invalid name".into();
                    }
                    Error::HeaderTooLarge => {
                        signed
                            .head
                            .headers
                            .push(Header::new("x-large", vec![b'a'; MAX_ENVELOPE_HEAD]));
                    }
                    _ => unreachable!(),
                }
                assert_eq!(
                    envelope_digest(&auth),
                    Err(expected),
                    "hops={hops:?} position={position}"
                );
            }
        }
    }
}
