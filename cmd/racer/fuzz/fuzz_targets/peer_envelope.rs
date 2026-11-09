//! Decode peer envelopes, signed heads, and `racer-chains` attachments
//! without verifying proofs.
#![no_main]

use http1::Codec;
use libfuzzer_sys::fuzz_target;
use std::sync::Arc;

/// Resolve every fingerprint, so decoding continues past chain resolution.
struct AnyFingerprint(Arc<peer_wire::Chain>);

impl peer_wire::Chains for AnyFingerprint {
    fn chain(&self, _: &[u8; 32]) -> Option<Arc<peer_wire::Chain>> {
        Some(self.0.clone())
    }
}

fuzz_target!(|data: &[u8]| {
    let _ = peer_wire::decode_signed(data);
    let _ = peer_wire::decode_binary(data);
    let _ = peer_wire::decode_nodes(data);
    let _ = peer_wire::Chain::decode(data);
    let _ = peer_wire::decode_attachment(data);
    if let Ok(Some((head, _))) = Codec::<()>::new(64 * 1024).decode_head(data) {
        let response = matches!(head.start, http1::StartLine::Response { .. });
        let chains = AnyFingerprint(peer_wire::Chain::decode(&[0, 0, 0, 1, 0]).unwrap());
        let _ = peer_wire::decode_envelope(head, response, &chains);
    }
});
