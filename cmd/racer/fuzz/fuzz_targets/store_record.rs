//! Decode disk record headers from whole records and from header prefixes.
#![no_main]

use libfuzzer_sys::fuzz_target;
use page_alloc::Extent;

fuzz_target!(|data: &[u8]| {
    let Ok(extent) = Extent::new(0, data.len()) else {
        return;
    };
    let _ = racer::store::parse_bytes(data, extent);
    // A prefix of a longer extent exercises header-only validation.
    if let Ok(longer) = Extent::new(0, data.len().saturating_mul(2).max(4096)) {
        let _ = racer::store::parse_prefix(data, longer);
    }
});
