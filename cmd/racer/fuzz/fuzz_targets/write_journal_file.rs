//! Replay arbitrary bytes as a whole write journal file, as worker startup
//! does: a torn or corrupt tail is dropped, and the folded live state must
//! rebuild itself from the records a compaction would write.
#![no_main]

use libfuzzer_sys::fuzz_target;
use racer::store::journal::{JournalState, fold_file};

fuzz_target!(|data: &[u8]| {
    let opened = fold_file(data, 0, &[0; 32]);
    assert!(opened.torn_bytes <= data.len() as u64);
    if opened.layout_changed {
        assert!(opened.state.pages.is_empty());
    } else {
        assert!(opened.unbound_pages.is_empty());
    }
    let state = &opened.state;
    let rebuilt = JournalState::from_records(&state.records(state.sequence));
    assert_eq!(&rebuilt, state);
});
