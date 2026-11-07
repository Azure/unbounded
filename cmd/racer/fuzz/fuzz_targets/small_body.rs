//! Feed arbitrary bytes to the small-body decoder in fuzzer-chosen fragments.
#![no_main]

use http1::small::{Decoder, Framing, Limits, Status};
use libfuzzer_sys::fuzz_target;

fuzz_target!(|data: &[u8]| {
    let Some((&control, rest)) = data.split_first() else {
        return;
    };
    let framing = match control % 3 {
        0 => Framing::Chunked,
        1 => Framing::Close,
        _ => Framing::Length(u64::from(control)),
    };
    let limits = Limits {
        body: 256,
        line: 32,
        trailers: 64,
    };
    let Ok(mut decoder) = Decoder::new(framing, limits) else {
        return;
    };
    let step = usize::from(control / 3 % 7) + 1;
    for part in rest.chunks(step) {
        match decoder.feed(part) {
            Ok(Status::Need) => {}
            Ok(Status::Done(used)) => {
                assert!(used <= part.len());
                break;
            }
            Err(_) => return,
        }
    }
    let _ = decoder.finish();
    assert!(decoder.body().len() <= limits.body);
});
