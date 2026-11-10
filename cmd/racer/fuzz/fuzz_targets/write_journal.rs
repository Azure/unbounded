//! Decode arbitrary bytes as write journal records, headers, manifests, and
//! holder tables; accepted records must re-encode to the same bytes.
#![no_main]

use libfuzzer_sys::fuzz_target;
use racer_core::write::{Holders, journal};

fuzz_target!(|data: &[u8]| {
    let (records, used) = journal::replay(data);
    assert!(used <= data.len());
    let mut encoded = Vec::new();
    for record in &records {
        journal::encode(record, &mut encoded).expect("decoded record re-encodes");
    }
    assert_eq!(encoded, data[..used]);
    let _ = journal::decode_header(data);
    if let Ok(manifest) = journal::decode_manifest(data) {
        let mut out = Vec::new();
        journal::encode_manifest(&manifest, &mut out).expect("manifest re-encodes");
        assert_eq!(out, data);
    }
    if let Ok(holders) = Holders::decode(data) {
        assert_eq!(holders.to_bytes().as_deref(), Ok(data));
    }
});
