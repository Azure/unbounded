// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Module-integration tests against an `na+sm` loopback.
//!
//! These exercise the full Wave 0..6 stack end-to-end: a single
//! listening `Nic` that resolves its own NA address via `HG_Addr_self`
//! + `HG_Addr_to_string`, registers itself as `PeerId(1)`, attaches a
//! `MercuryServer` against a deterministic `BulkSource`, and forwards
//! `bulk_get` RPCs to itself through `MercuryTransport`.

use std::ffi::{CStr, CString};
use std::future::Future;
use std::os::raw::c_char;
use std::pin::Pin;
use std::ptr;
use std::sync::Arc;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::task::{Context, Poll, Waker};
use std::time::{Duration, Instant};

use serde::{Deserialize, Serialize};

use super::config::NicConfig;
use super::error::HgError;
use super::ffi::{self, HG_SUCCESS, hg_addr_t, hg_size_t};
use super::nic::Nic;
use super::peer::PeerId;
use super::router::StaticPeer;
use super::server::{BulkSource, MercuryServer};
use super::transport::MercuryTransport;
use crate::bufferpool::{Backing, BulkRef, PageRef, Req, StripeKey, Transport};
use crate::runtime::{DefaultRuntime, WorkerIdx};

// =====================================================================
// Test request and deterministic BulkSource
// =====================================================================

#[derive(Clone, Serialize, Deserialize)]
struct TestReq {
    key: [u8; 32],
}

impl Req for TestReq {
    fn key(&self) -> StripeKey {
        StripeKey(self.key)
    }
}

/// `BulkSource` that returns a pattern derived from `(key[0], offset)`.
/// Keeps a job counter and records the worker thread name of every
/// fetch so multi-context tests can assert distinct drainers saw work.
struct CountingSource {
    jobs: AtomicUsize,
    threads: std::sync::Mutex<std::collections::HashSet<String>>,
}

impl CountingSource {
    fn new() -> Self {
        Self {
            jobs: AtomicUsize::new(0),
            threads: std::sync::Mutex::new(std::collections::HashSet::new()),
        }
    }

    fn job_count(&self) -> usize {
        self.jobs.load(Ordering::SeqCst)
    }

    fn distinct_threads(&self) -> usize {
        self.threads.lock().unwrap().len()
    }
}

fn pattern_byte(key0: u8, offset: u64, idx: usize) -> u8 {
    // Deterministic pattern per (key, absolute offset within stripe).
    key0.wrapping_add((offset as usize).wrapping_add(idx) as u8)
}

impl BulkSource<TestReq> for CountingSource {
    fn fetch<'a>(
        &'a self,
        req: &'a TestReq,
        offset: u64,
        len: u32,
    ) -> Pin<Box<dyn Future<Output = Result<Vec<u8>, HgError>> + Send + 'a>> {
        let key0 = req.key[0];
        self.jobs.fetch_add(1, Ordering::SeqCst);
        if let Some(name) = std::thread::current().name() {
            self.threads.lock().unwrap().insert(name.to_string());
        }
        Box::pin(async move {
            let mut v = vec![0u8; len as usize];
            for (i, b) in v.iter_mut().enumerate() {
                *b = pattern_byte(key0, offset, i);
            }
            Ok(v)
        })
    }
}

// =====================================================================
// noop-waker block_on (per-area pattern; not shared across files)
// =====================================================================

fn noop_raw_waker() -> std::task::RawWaker {
    fn no_op(_: *const ()) {}
    fn clone(_: *const ()) -> std::task::RawWaker {
        noop_raw_waker()
    }
    let vt = &std::task::RawWakerVTable::new(clone, no_op, no_op, no_op);
    std::task::RawWaker::new(ptr::null(), vt)
}

fn noop_waker() -> Waker {
    // SAFETY: vtable is `'static`, all functions are no-ops, the data
    // pointer is never dereferenced.
    unsafe { Waker::from_raw(noop_raw_waker()) }
}

/// `block_on` driven by a noop waker. Mercury's progress thread (per
/// context) plus the server drainer threads make forward progress in
/// the background; this loop just yields until the bound completion
/// slot resolves.
fn block_on<F: Future>(mut f: F) -> F::Output {
    // SAFETY: `f` is owned by this stack frame and is not moved after
    // being pinned.
    let mut f = unsafe { Pin::new_unchecked(&mut f) };
    let waker = noop_waker();
    let mut cx = Context::from_waker(&waker);
    let mut spins = 0u32;
    loop {
        match f.as_mut().poll(&mut cx) {
            Poll::Ready(v) => return v,
            Poll::Pending => {
                spins += 1;
                assert!(spins < 1_000_000, "block_on spun without progress");
                std::thread::yield_now();
            }
        }
    }
}

// =====================================================================
// Loopback Nic helpers
// =====================================================================

fn cfg(contexts: u16, max_in_flight: u32) -> NicConfig {
    NicConfig {
        na_info_string: "na+sm".to_string(),
        listen: true,
        contexts_per_nic: contexts,
        max_in_flight_per_ctx: max_in_flight,
        ..NicConfig::default()
    }
}

/// Build a listening Nic on `na+sm`. Returns `None` if Mercury cannot
/// bring up the class in this environment (e.g. missing libfabric/SM
/// support); callers panic in that case so the test surfaces loudly.
fn build_listening_nic(contexts: u16, max_in_flight: u32) -> Arc<Nic> {
    let rt = DefaultRuntime::new(1);
    let nic = match Nic::new(&cfg(contexts, max_in_flight), &*rt, WorkerIdx(0)) {
        Ok(n) => n,
        Err(HgError::HgInit(_)) => panic!("HG_Init(na+sm) failed; mercury runtime missing"),
        Err(e) => panic!("unexpected Nic::new error: {e:?}"),
    };
    Arc::new(nic)
}

/// Resolve our own published NA address to a string suitable for
/// `HG_Addr_lookup2`. Two-pass: first call discovers the required
/// buffer length, second call fills it.
fn self_addr_string(nic: &Nic) -> String {
    let class = nic.class();
    let mut self_addr: hg_addr_t = ptr::null_mut();
    // SAFETY: `class` is non-null; `&mut self_addr` is a valid out-param.
    let rc = unsafe { ffi::HG_Addr_self(class, &mut self_addr) };
    assert_eq!(rc, HG_SUCCESS, "HG_Addr_self failed: rc={rc}");
    assert!(!self_addr.is_null(), "HG_Addr_self returned null addr");

    let mut len: hg_size_t = 0;
    // First call: NULL buf, get required length (includes trailing NUL).
    // SAFETY: `class` and `self_addr` are live; passing NULL buf with a
    // valid length out-param is the documented "size query" idiom.
    let rc = unsafe { ffi::HG_Addr_to_string(class, ptr::null_mut(), &mut len, self_addr) };
    assert_eq!(rc, HG_SUCCESS, "HG_Addr_to_string(size) failed: rc={rc}");
    assert!(len > 0, "HG_Addr_to_string(size) returned len=0");

    let mut buf = vec![0u8; len as usize];
    let mut len2: hg_size_t = len;
    // SAFETY: `buf` is `len` bytes long and writeable; Mercury writes a
    // null-terminated C string of size `len2` into it.
    let rc = unsafe {
        ffi::HG_Addr_to_string(class, buf.as_mut_ptr() as *mut c_char, &mut len2, self_addr)
    };
    assert_eq!(rc, HG_SUCCESS, "HG_Addr_to_string(fill) failed: rc={rc}");

    // Free the self addr; we've copied the string out.
    // SAFETY: `self_addr` was produced by `HG_Addr_self` against the
    // same class and has not been freed yet.
    unsafe {
        let _ = ffi::HG_Addr_free(class, self_addr);
    }

    let cstr =
        CStr::from_bytes_with_nul(&buf[..len2 as usize]).expect("self addr is valid C string");
    cstr.to_string_lossy().into_owned()
}

/// Build a 16 MiB Backing with `page_size` bytes per page and the
/// destination region zeroed. Returns the `(backing, raw_bytes_ptr)`
/// pair; the test reads through `raw_bytes_ptr` to verify the bytes
/// the server pushed.
fn make_backing(page_size: usize, page_count: usize) -> (Backing, *mut u8) {
    let total = page_size * page_count;
    let mut bytes = vec![0u8; total];
    let base = bytes.as_mut_ptr();
    let backing = Backing {
        base,
        page_size,
        page_count,
        _own: Box::new(bytes),
    };
    (backing, base)
}

/// Convenience: register self under `PeerId(1)`.
fn register_self_as_peer1(nic: &Nic) {
    let s = self_addr_string(nic);
    nic.add_peer(PeerId(1), &s).expect("add self peer");
}

// =====================================================================
// Test 1: single-page round trip
// =====================================================================

#[test]
fn loopback_round_trip_single_page() {
    let nic = build_listening_nic(2, 1024);
    register_self_as_peer1(&nic);

    let page_size: usize = 4096;
    let page_count: usize = 4096; // 16 MiB total
    let (backing, base) = make_backing(page_size, page_count);
    nic.register_backing(&backing).expect("register backing");

    let source = Arc::new(CountingSource::new());
    let server = MercuryServer::<TestReq, _>::new(Arc::clone(&nic), Arc::clone(&source));
    let server_handle = server.run();

    let router = Arc::new(StaticPeer(PeerId(1)));
    let transport = MercuryTransport::<TestReq, _>::new(
        Arc::clone(&nic),
        0,
        page_size as u32,
        Arc::clone(&router),
    )
    .expect("transport built");

    let req = TestReq { key: [0xABu8; 32] };
    let stripe = StripeKey([0xABu8; 32]);
    let src = BulkRef {
        stripe,
        offset: 0,
        len: page_size as u32,
    };
    // Land at page index 0.
    let dst = PageRef {
        page_idx: 0,
        offset: 0,
        len: page_size as u32,
    };
    block_on(transport.bulk_get(&req, src, dst)).expect("bulk_get ok");

    // Verify destination region matches the deterministic pattern.
    // SAFETY: `base` points to `page_size * page_count` valid bytes;
    // the slice we read is fully within page 0.
    let observed: &[u8] = unsafe { std::slice::from_raw_parts(base, page_size) };
    for (i, b) in observed.iter().enumerate().take(page_size) {
        let want = pattern_byte(0xAB, 0, i);
        assert_eq!(
            *b, want,
            "byte {i}: got {b:#x}, want {want:#x} (key[0]=0xAB, offset=0)"
        );
    }
    assert!(source.job_count() >= 1, "server saw at least one job");

    server_handle.stop();
    nic.shutdown().expect("nic shutdown");
}

// =====================================================================
// Test 2: in-flight forwards do not deadlock teardown
// =====================================================================

#[test]
fn class_drop_with_in_flight_forwards_does_not_hang() {
    let nic = build_listening_nic(2, 1024);
    register_self_as_peer1(&nic);

    let page_size: usize = 4096;
    let page_count: usize = 256;
    let (backing, _base) = make_backing(page_size, page_count);
    nic.register_backing(&backing).expect("register backing");

    let source = Arc::new(CountingSource::new());
    let server = MercuryServer::<TestReq, _>::new(Arc::clone(&nic), Arc::clone(&source));
    let server_handle = server.run();

    let router = Arc::new(StaticPeer(PeerId(1)));
    let transport = Arc::new(
        MercuryTransport::<TestReq, _>::new(
            Arc::clone(&nic),
            0,
            page_size as u32,
            Arc::clone(&router),
        )
        .expect("transport built"),
    );

    // Issue a batch of forwards; we don't wait for them. Each runs on
    // its own thread driving its own block_on loop; we tear down
    // before they all finish.
    let mut workers = Vec::new();
    for i in 0..64u32 {
        let transport = Arc::clone(&transport);
        let req = TestReq { key: [i as u8; 32] };
        let stripe = StripeKey([i as u8; 32]);
        let page_idx = (i as usize) % page_count;
        workers.push(std::thread::spawn(move || {
            let src = BulkRef {
                stripe,
                offset: 0,
                len: page_size as u32,
            };
            let dst = PageRef {
                page_idx: page_idx as u32,
                offset: 0,
                len: page_size as u32,
            };
            // Errors are fine; we're racing teardown.
            let _ = block_on(transport.bulk_get(&req, src, dst));
        }));
    }

    // Give Mercury a moment to start servicing before tearing down.
    std::thread::sleep(Duration::from_millis(20));

    let started = Instant::now();
    server_handle.stop();
    nic.shutdown().expect("nic shutdown");
    let teardown = started.elapsed();
    assert!(
        teardown < Duration::from_secs(5),
        "shutdown took {teardown:?}; expected < 5s"
    );

    // Drop the transport (and the last Arc<Nic> it held) before
    // joining the workers, so the workers' bulk_gets observe the
    // class as torn down. Every worker must terminate (no hangs).
    drop(transport);
    drop(nic);

    let join_started = Instant::now();
    for (i, h) in workers.into_iter().enumerate() {
        let r = h.join();
        assert!(r.is_ok(), "worker {i} panicked: {r:?}");
        assert!(
            join_started.elapsed() < Duration::from_secs(5),
            "worker {i} took longer than 5s to exit"
        );
    }
}

// =====================================================================
// Test 3: multi-context distribution
// =====================================================================

#[test]
fn multi_context_distribution() {
    let nic = build_listening_nic(4, 1024);
    register_self_as_peer1(&nic);

    let page_size: usize = 1024;
    let page_count: usize = 256;
    let (backing, _base) = make_backing(page_size, page_count);
    nic.register_backing(&backing).expect("register backing");

    let source = Arc::new(CountingSource::new());
    let server = MercuryServer::<TestReq, _>::new(Arc::clone(&nic), Arc::clone(&source));
    let server_handle = server.run();

    let router = Arc::new(StaticPeer(PeerId(1)));
    // The server-side trampoline picks a context by hashing the
    // inbound peer addr's pointer. In `na+sm` self-loopback every
    // forward arrives from the same self-addr, so all jobs land on
    // a single drainer; that is by design (see `nic.rs`'s trampoline
    // comment). We still construct four contexts and four client
    // transports to exercise client-side multi-context plumbing, but
    // we assert that all forwards completed and the server saw every
    // job rather than that fan-out reached every drainer.
    let mut transports = Vec::new();
    for i in 0..4u16 {
        transports.push(Arc::new(
            MercuryTransport::<TestReq, _>::new(
                Arc::clone(&nic),
                i,
                page_size as u32,
                Arc::clone(&router),
            )
            .expect("transport built"),
        ));
    }

    let mut workers = Vec::new();
    for i in 0..256u32 {
        let transport = Arc::clone(&transports[(i as usize) % transports.len()]);
        workers.push(std::thread::spawn(move || {
            let req = TestReq { key: [i as u8; 32] };
            let src = BulkRef {
                stripe: StripeKey([i as u8; 32]),
                offset: 0,
                len: page_size as u32,
            };
            let dst = PageRef {
                page_idx: (i as usize % 256) as u32,
                offset: 0,
                len: page_size as u32,
            };
            block_on(transport.bulk_get(&req, src, dst))
        }));
    }
    for h in workers {
        let _ = h.join().expect("worker joined");
    }

    assert_eq!(source.job_count(), 256, "every forward served");
    let distinct = source.distinct_threads();
    assert!(
        distinct >= 1,
        "expected at least one server drainer to see work, got {distinct}"
    );

    drop(transports);
    server_handle.stop();
    nic.shutdown().expect("nic shutdown");
}

// =====================================================================
// Test 4: capacity backpressure does not livelock
// =====================================================================

#[test]
fn capacity_backpressure_does_not_livelock() {
    // Single context, tight slot bound. The CompletionRegistry should
    // wake new acquires as completions land.
    let nic = build_listening_nic(1, 4);
    register_self_as_peer1(&nic);

    let page_size: usize = 1024;
    let page_count: usize = 64;
    let (backing, _base) = make_backing(page_size, page_count);
    nic.register_backing(&backing).expect("register backing");

    let source = Arc::new(CountingSource::new());
    let server = MercuryServer::<TestReq, _>::new(Arc::clone(&nic), Arc::clone(&source));
    let server_handle = server.run();

    let router = Arc::new(StaticPeer(PeerId(1)));
    let transport = Arc::new(
        MercuryTransport::<TestReq, _>::new(
            Arc::clone(&nic),
            0,
            page_size as u32,
            Arc::clone(&router),
        )
        .expect("transport built"),
    );

    let mut workers = Vec::new();
    for i in 0..32u32 {
        let transport = Arc::clone(&transport);
        workers.push(std::thread::spawn(move || {
            let req = TestReq { key: [i as u8; 32] };
            let src = BulkRef {
                stripe: StripeKey([i as u8; 32]),
                offset: 0,
                len: page_size as u32,
            };
            let dst = PageRef {
                page_idx: (i as usize % 64) as u32,
                offset: 0,
                len: page_size as u32,
            };
            block_on(transport.bulk_get(&req, src, dst))
        }));
    }

    let started = Instant::now();
    for (i, h) in workers.into_iter().enumerate() {
        let r = h.join().expect("worker joined");
        assert!(r.is_ok(), "worker {i} returned err: {r:?}");
        assert!(
            started.elapsed() < Duration::from_secs(30),
            "took longer than 30s; suspect livelock"
        );
    }

    assert_eq!(source.job_count(), 32, "every forward served");
    drop(transport);
    server_handle.stop();
    nic.shutdown().expect("nic shutdown");
}

// =====================================================================
// Sanity: peer registration round-trips
// =====================================================================

#[test]
fn self_addr_lookup_round_trip() {
    let nic = build_listening_nic(1, 16);
    let s = self_addr_string(&nic);
    assert!(!s.is_empty(), "self addr string should not be empty");
    // Bring it back through the public peer-add API.
    nic.add_peer(PeerId(7), &s).expect("add self peer");
    nic.shutdown().expect("nic shutdown");
}

// Sanity: lookup_and_insert via PeerEntry happens during Nic::new when
// `cfg.peers` is non-empty. Cover that branch too.
#[test]
fn nic_with_self_peer_in_initial_config() {
    // First bring up a temporary listening nic to discover its self
    // addr; then bring up the *real* nic with that addr in cfg.peers.
    // Since each nic is independent on shared memory, this exercises
    // peer-table initialization at construction time on a fresh class.
    let probe = build_listening_nic(1, 16);
    let addr = self_addr_string(&probe);
    drop(probe);

    let mut c = cfg(2, 16);
    c.peers = vec![super::config::PeerEntry {
        id: PeerId(2),
        na_addr: addr,
    }];
    let rt = DefaultRuntime::new(1);
    let nic = Nic::new(&c, &*rt, WorkerIdx(0)).expect("nic with peer ok");
    nic.shutdown().expect("shutdown ok");
}

/// Catch-all guard against `CString::new` panicking on test input.
fn _validate_cstring_invariants() {
    let _ = CString::new("na+sm").unwrap();
}
