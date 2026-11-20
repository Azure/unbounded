//! Decode arbitrary bytes as an HTTP/1.1 head, then parse accepted write heads.
#![no_main]

use libfuzzer_sys::fuzz_target;
use racer_wire::http::{Codec, Opaque};
use racer_wire::object::PageSize;
use racer_wire::object::client::MAX_HEAD_BYTES;
use racer_wire::object::write::{WriteKind, is_write, max_object_bytes, parse_write};

/// Opaque policy matching the strict Racer fields.
struct Strict;

impl Opaque for Strict {
    const NAMES: &'static [&'static str] = &["authorization", "racer-metadata"];
}

fuzz_target!(|data: &[u8]| {
    let Some((&control, data)) = data.split_first() else {
        return;
    };
    let Ok(page) = PageSize::from_wire(4096 << (control % 13)) else {
        return;
    };
    let Ok(Some((head, _))) = Codec::<Strict>::new(MAX_HEAD_BYTES).decode_head(data) else {
        return;
    };
    let routed = is_write(&head);
    if let Ok(request) = parse_write(&head, MAX_HEAD_BYTES, page) {
        assert!(routed, "parsed a head that is not routed as a write");
        if let Some(length) = request.kind.object_length() {
            assert!(length > 0 && length <= max_object_bytes(page));
        }
        if let WriteKind::HostWrite { source } = request.kind {
            assert!(source.offset.checked_add(source.length).is_some());
        }
    }
});
