//! Decode arbitrary bytes as an HTTP/1.1 head, then re-encode accepted heads.
#![no_main]

use http1::{Codec, Opaque};
use libfuzzer_sys::fuzz_target;

/// Opaque policy matching the strict Racer fields.
struct Strict;

impl Opaque for Strict {
    const NAMES: &'static [&'static str] = &["authorization", "racer-metadata"];
}

/// Decode, then require that every accepted head is syntactically re-encodable.
fn check<O: Opaque>(codec: Codec<O>, data: &[u8]) {
    if let Ok(Some((head, used))) = codec.decode_head(data) {
        assert!(used <= data.len());
        let _ = head.closes_connection();
        // Encoding adds one separator space per field, so it may exceed the
        // wire limit, but an accepted head is always syntactically encodable.
        match codec.encode_head(&head) {
            Ok(_) | Err(http1::Error::HeadTooLarge) => {}
            Err(error) => panic!("decoded head failed to re-encode: {error}"),
        }
    }
}

fuzz_target!(|data: &[u8]| {
    let Some((&control, data)) = data.split_first() else {
        return;
    };
    let limit = 16 + usize::from(control) * 32;
    check(Codec::<()>::new(limit), data);
    check(Codec::<()>::new(limit).trim_ows(), data);
    check(Codec::<Strict>::new(limit), data);
    check(Codec::<Strict>::new(limit).trim_ows(), data);
});
