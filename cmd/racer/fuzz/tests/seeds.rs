//! Keep the committed seed corpora decodable by the current parsers.
//!
//! `seeds_decode` fails when a seed drifts from its wire format. Regenerate
//! the encoder-built seeds with:
//!
//! ```sh
//! RUSTC_BOOTSTRAP=1 cargo +1.96.0 test --test seeds -- --ignored regenerate
//! ```

use http1::{Codec, Header, MessageHead, StartLine};
use page_alloc::Extent;
use peer_wire::{Chain, Chains, ForwardedHead, SignedHead};
use racer_wire::control;
use std::path::{Path, PathBuf};
use std::sync::Arc;

const CLUSTER: &str = "11111111-1111-4111-8111-111111111111";

/// Resolve every fingerprint, as the `peer_envelope` target does.
struct AnyFingerprint(Arc<Chain>);

impl Chains for AnyFingerprint {
    fn chain(&self, _: &[u8; 32]) -> Option<Arc<Chain>> {
        Some(self.0.clone())
    }
}

fn corpus(target: &str) -> PathBuf {
    Path::new(env!("CARGO_MANIFEST_DIR"))
        .join("corpus")
        .join(target)
}

fn read(target: &str, name: &str) -> Vec<u8> {
    let path = corpus(target).join(name);
    std::fs::read(&path).unwrap_or_else(|e| panic!("{}: {e}", path.display()))
}

fn seeds(target: &str) -> Vec<(String, Vec<u8>)> {
    let mut out: Vec<_> = std::fs::read_dir(corpus(target))
        .unwrap()
        .map(|entry| entry.unwrap())
        .filter(|entry| entry.file_name().to_string_lossy().starts_with("seed-"))
        .map(|entry| {
            (
                entry.file_name().to_string_lossy().into_owned(),
                std::fs::read(entry.path()).unwrap(),
            )
        })
        .collect();
    out.sort();
    assert!(!out.is_empty(), "no seeds for {target}");
    out
}

/// One proof head carrying the signed certificate fingerprint field.
fn proof(kind: &str) -> SignedHead {
    let fingerprint = base64(&[7; 32]);
    SignedHead {
        head: MessageHead {
            start: StartLine::Request {
                method: "GET".into(),
                target: "/v1/objects/x".into(),
            },
            headers: vec![
                Header::new("host", b"racer"),
                Header::new("racer-kind", kind.as_bytes()),
                Header::new("racer-certificate", fingerprint.as_bytes()),
            ],
        },
        signature: vec![1; 64],
        chain: None,
    }
}

/// Padded standard base64, the binary field encoding of the peer wire.
fn base64(bytes: &[u8]) -> String {
    const TABLE: &[u8; 64] = b"ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";
    let mut out = String::new();
    for chunk in bytes.chunks(3) {
        let b = [
            chunk[0],
            *chunk.get(1).unwrap_or(&0),
            *chunk.get(2).unwrap_or(&0),
        ];
        let n = u32::from(b[0]) << 16 | u32::from(b[1]) << 8 | u32::from(b[2]);
        for i in 0..4 {
            if i <= chunk.len() {
                out.push(TABLE[(n >> (18 - 6 * i) & 63) as usize] as char);
            } else {
                out.push('=');
            }
        }
    }
    out
}

fn unhex(hex: &str) -> Vec<u8> {
    hex.as_bytes()
        .chunks_exact(2)
        .map(|pair| u8::from_str_radix(std::str::from_utf8(pair).unwrap(), 16).unwrap())
        .collect()
}

/// Generated seeds, by target and file name.
fn generated() -> Vec<(&'static str, &'static str, Vec<u8>)> {
    let testdata = |name: &str| {
        let path = Path::new(env!("CARGO_MANIFEST_DIR"))
            .join("../wire/testdata")
            .join(name);
        std::fs::read(&path).unwrap_or_else(|e| panic!("{}: {e}", path.display()))
    };
    let mut out = vec![
        (
            "control_documents",
            "seed-publication",
            testdata("publication.json"),
        ),
        ("control_documents", "seed-bundle", testdata("bundle.json")),
        (
            "control_documents",
            "seed-bootstrap-request",
            testdata("bootstrap-request.json"),
        ),
        (
            "control_documents",
            "seed-bootstrap-response",
            testdata("bootstrap-response.json"),
        ),
        (
            "control_documents",
            "seed-error",
            control::encode_error(&control::ErrorResponse {
                code: control::ProtocolFailure::InvalidRequest,
            })
            .unwrap(),
        ),
    ];

    // Peer wire: a request and a response envelope with one hop, a bare signed
    // proof, and an encoded certificate chain.
    let auth = ForwardedHead {
        original: Arc::new(proof("original")),
        hops: vec![proof("hop")],
    };
    let codec = Codec::<()>::new(peer_wire::MAX_HEAD);
    let request = peer_wire::encode_envelope(&auth, false, 0).unwrap();
    let response = peer_wire::encode_envelope(&auth, true, 5).unwrap();
    out.push((
        "peer_envelope",
        "seed-envelope",
        codec.encode_head(&request).unwrap(),
    ));
    out.push((
        "peer_envelope",
        "seed-envelope-response",
        codec.encode_head(&response).unwrap(),
    ));
    out.push((
        "peer_envelope",
        "seed-signed",
        peer_wire::encode_signed(&auth.original).unwrap(),
    ));
    let chain = Chain::new(&[vec![0x30, 0x03, 0x02, 0x01, 0x01]]).unwrap();
    out.push(("peer_envelope", "seed-chain", chain.encoded().to_vec()));
    out.push((
        "peer_envelope",
        "seed-attachment",
        peer_wire::encode_attachment(&[chain]).unwrap().into_bytes(),
    ));

    // Checkpoint image with one empty shard.
    let geometry = page_alloc::SegmentGeometry::new(
        1 << 20,
        1 << 18,
        4,
        page_alloc::Alignment::new(512, 512, 512).unwrap(),
    )
    .unwrap();
    let segments = page_alloc::Segments::from_geometry(geometry).unwrap();
    out.push((
        "checkpoint",
        "seed-empty",
        racer::store::checkpoint::encode(&racer::store::checkpoint::CheckpointImage {
            version: racer::store::checkpoint::CHECKPOINT_VERSION,
            page_size: racer_wire::object::PageSize::DEFAULT,
            cluster: control::ClusterId(CLUSTER.into()),
            sequence: 1,
            shards: vec![racer::store::checkpoint::ShardImage {
                worker: racer_wire::object::WorkerId(0),
                geometry: geometry.into(),
                index: racer::store::catalog::IndexSnapshot {
                    entries: vec![],
                    metadata: vec![],
                },
                segments: segments.snapshot(),
                holds: vec![],
            }],
        })
        .unwrap(),
    ));

    // Store record: the format 5 golden vector from the racer store tests
    // (`current_record_matches_golden_vector`), laid out in a 1 KiB extent.
    let header = unhex(concat!(
        "5243525041474531050000000002000007000000000000000300000000000000",
        "0000000000000000030000001300000001010101010101010101010101010101",
        "0202020202020202020202020202020202020202020202020303030303030303",
        "0303030303030303030303030303030303030303030303030500000004000000",
        "6361636865227631220000000001000000000000015ee411e2bc3d065d34e43a",
        "0c64935a2cd3e4e7c727f5a86a0aebaa8527b4aec2",
    ));
    let ciphertext = unhex("02020202020202020202020202020202020202");
    let mut record = vec![0; 1024];
    record[..header.len()].copy_from_slice(&header);
    record[512..512 + ciphertext.len()].copy_from_slice(&ciphertext);
    out.push(("store_record", "seed-record", record));
    out
}

#[test]
#[ignore = "writes the corpus; run explicitly to regenerate seeds"]
fn regenerate() {
    for (target, name, bytes) in generated() {
        std::fs::create_dir_all(corpus(target)).unwrap();
        std::fs::write(corpus(target).join(name), bytes).unwrap();
    }
}

#[test]
fn generated_seeds_are_current() {
    for (target, name, bytes) in generated() {
        assert!(
            read(target, name) == bytes,
            "{target}/{name} is stale; run the ignored `regenerate` test"
        );
    }
}

#[test]
fn seeds_decode() {
    for (name, data) in seeds("control_documents") {
        let ok = control::decode_publication(&data).is_ok()
            || control::decode_bundle(&data).is_ok()
            || control::decode_error(&data).is_ok()
            || control::decode_enrollment_request(&data).is_ok()
            || control::decode_enrollment_response(&data).is_ok();
        assert!(ok, "control_documents/{name} does not decode");
    }
    assert!(control::decode_publication(&read("control_documents", "seed-publication")).is_ok());
    assert!(control::decode_bundle(&read("control_documents", "seed-bundle")).is_ok());

    let chains = AnyFingerprint(Chain::decode(&[0, 0, 0, 1, 0]).unwrap());
    for (name, data) in seeds("peer_envelope") {
        let ok = peer_wire::decode_signed(&data).is_ok()
            || Chain::decode(&data).is_ok()
            || peer_wire::decode_attachment(&data).is_ok()
            || match Codec::<()>::new(64 * 1024).decode_head(&data) {
                Ok(Some((head, _))) => {
                    let response = matches!(head.start, StartLine::Response { .. });
                    peer_wire::decode_envelope(head, response, &chains).is_ok()
                }
                _ => false,
            };
        assert!(ok, "peer_envelope/{name} does not decode");
    }
    assert_eq!(
        peer_wire::decode_attachment(&read("peer_envelope", "seed-attachment"))
            .unwrap()
            .len(),
        1
    );

    for (name, data) in seeds("checkpoint") {
        racer::store::checkpoint::decode(&data)
            .unwrap_or_else(|e| panic!("checkpoint/{name}: {e:?}"));
    }

    for (name, data) in seeds("store_record") {
        racer::store::parse_bytes(&data, Extent::new(0, data.len()).unwrap())
            .unwrap_or_else(|e| panic!("store_record/{name}: {e:?}"));
    }

    for (name, data) in seeds("http_head") {
        let (_, head) = data.split_first().unwrap();
        assert!(
            matches!(Codec::<()>::new(64 * 1024).decode_head(head), Ok(Some(_))),
            "http_head/{name} does not decode"
        );
    }
}
