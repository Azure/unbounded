//! Single-core page crypto throughput baseline on 16 MiB pages.
//!
//! Run pinned to one CPU:
//! `taskset -c 2 cargo test --release -p racer-crypto --test throughput -- --ignored --nocapture`
use racer_crypto::TAG_LEN;
use racer_crypto::copy_prefix_crc64;
use racer_crypto::crc64;
use racer_crypto::open;
use racer_crypto::seal;
use std::hint::black_box;
use std::time::Instant;

const PAGE: usize = 16 << 20;
const GIB: f64 = (1u64 << 30) as f64;

fn rate(iterations: usize, mut body: impl FnMut(usize)) -> f64 {
    // One warmup iteration faults in pages and warms caches.
    body(0);
    let start = Instant::now();
    for i in 0..iterations {
        body(i + 1);
    }
    (iterations * PAGE) as f64 / GIB / start.elapsed().as_secs_f64()
}

#[test]
#[ignore = "release-only single-core crypto benchmark"]
fn page_crypto_throughput() {
    #[allow(clippy::assertions_on_constants)]
    {
        assert!(!cfg!(debug_assertions), "run with --release");
    }
    let iterations: usize = std::env::var("RACER_CRYPTO_BENCH_PAGES")
        .map_or(32, |value| value.parse().expect("unsigned page count"));
    let key = [7u8; 32];
    let aad = [3u8; 64];
    let plaintext: Vec<u8> = (0..PAGE).map(|i| (i % 251) as u8).collect();
    let mut sealed = vec![0u8; PAGE + TAG_LEN];
    let mut opened = vec![0u8; PAGE];
    let nonce = |i: usize| {
        let mut nonce = [0u8; 24];
        nonce[..8].copy_from_slice(&(i as u64).to_le_bytes());
        nonce
    };
    let seal_rate = rate(iterations, |i| {
        seal(&key, &nonce(i), &aad, &plaintext, &mut sealed).unwrap();
    });
    seal(&key, &nonce(0), &aad, &plaintext, &mut sealed).unwrap();
    let open_rate = rate(iterations, |_| {
        open(&key, &nonce(0), &aad, &sealed, &mut opened).unwrap();
    });
    assert_eq!(opened, plaintext);
    let crc_rate = rate(iterations, |_| {
        black_box(crc64(black_box(&sealed[..PAGE])));
    });
    let copy_crc_rate = rate(iterations, |_| {
        black_box(copy_prefix_crc64(black_box(&sealed[..PAGE]), &mut opened).unwrap());
    });
    let copy_rate = rate(iterations, |_| {
        opened.copy_from_slice(black_box(&sealed[..PAGE]));
        black_box(&opened);
    });
    println!(
        "CRYPTO_BENCH {{\"page_bytes\":{PAGE},\"pages\":{iterations},\"seal_gib_s\":{seal_rate:.3},\"open_gib_s\":{open_rate:.3},\"crc64_gib_s\":{crc_rate:.3},\"copy_prefix_crc64_gib_s\":{copy_crc_rate:.3},\"memcpy_gib_s\":{copy_rate:.3}}}"
    );
}
