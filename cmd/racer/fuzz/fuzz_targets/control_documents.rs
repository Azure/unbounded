//! Decode control-plane publications, keyring bundles, and related documents.
#![no_main]

use libfuzzer_sys::fuzz_target;

fuzz_target!(|data: &[u8]| {
    let _ = racer_wire::control::decode_publication(data);
    let _ = racer_wire::control::decode_bundle(data);
    let _ = racer_wire::control::decode_error(data);
    let _ = racer_wire::control::decode_enrollment_request(data);
    let _ = racer_wire::control::decode_enrollment_response(data);
});
