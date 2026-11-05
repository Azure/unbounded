//! Decode peer envelopes and signed heads without verifying proofs.
#![no_main]

use http1::Codec;
use libfuzzer_sys::fuzz_target;

fuzz_target!(|data: &[u8]| {
    let _ = peer_wire::decode_signed(data);
    let _ = peer_wire::decode_binary(data);
    let _ = peer_wire::decode_nodes(data);
    if let Ok(Some((head, _))) = Codec::<()>::new(64 * 1024).decode_head(data) {
        let response = matches!(head.start, http1::StartLine::Response { .. });
        let _ = peer_wire::decode_envelope(head, response);
    }
});
