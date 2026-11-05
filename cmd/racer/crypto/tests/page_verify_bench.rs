//! Tag-only XAES-256-GCM verification versus a full open and a plain copy,
//! on 16 MiB pages.
//!
//! Run with:
//! `cargo test --release -p racer-crypto --test page_verify_bench -- --ignored --nocapture`

use std::hint::black_box;
use std::time::{Duration, Instant};

use racer_crypto::{TAG_LEN, open, seal, verify_tag};

const PAGE: usize = 16 << 20;
const ROUNDS: usize = 24;

fn gib_per_sec(elapsed: Duration) -> f64 {
    (PAGE * ROUNDS) as f64 / elapsed.as_secs_f64() / f64::from(1u32 << 30)
}

fn time(mut work: impl FnMut()) -> Duration {
    // One warm-up pass, then the timed rounds.
    work();
    let start = Instant::now();
    for _ in 0..ROUNDS {
        work();
    }
    start.elapsed()
}

#[test]
#[ignore = "release-mode throughput benchmark"]
fn tag_verify_versus_open_and_memcpy_on_16_mib_pages() {
    let key = [7u8; 32];
    let nonce = [9u8; 24];
    let aad = [3u8; 96];
    let plaintext: Vec<u8> = (0..PAGE).map(|i| (i * 31 + 7) as u8).collect();
    let mut sealed = vec![0u8; PAGE + TAG_LEN];
    seal(&key, &nonce, &aad, &plaintext, &mut sealed).unwrap();
    let (body, tag) = sealed.split_at(PAGE);
    let mut out = vec![0u8; PAGE];

    let copy = time(|| {
        out.copy_from_slice(black_box(body));
        black_box(&out);
    });
    let tag_verify = time(|| {
        verify_tag(&key, &nonce, &[&aad], black_box(body), tag).unwrap();
    });
    let full = time(|| {
        open(&key, &nonce, &aad, black_box(&sealed), &mut out).unwrap();
    });
    let (copy, tag_verify, full) = (
        gib_per_sec(copy),
        gib_per_sec(tag_verify),
        gib_per_sec(full),
    );
    eprintln!(
        "memcpy {copy:.2} GiB/s, verify_tag {tag_verify:.2} GiB/s, open {full:.2} GiB/s, \
         open time / tag time {:.3}",
        tag_verify / full
    );
}
