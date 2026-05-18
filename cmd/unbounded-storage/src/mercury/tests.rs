// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Module integration tests for the Mercury transport. Use Mercury's
//! `na+sm://` (shared-memory) NA backend so the tests can run without
//! any specialized hardware. The suite exercises:
//!
//! * happy-path bulk_get round trips,
//! * routing / address / registration error paths,
//! * server-side error propagation (decode, fetch, short read, over-production),
//! * page-offset flattening with non-zero `page_idx` and `offset`,
//! * concurrent in-flight RPCs and registry-full back-pressure,
//! * teardown orderings (class-first vs no-traffic; server exits on close),
//! * a stress loop that pushes 5,000 round trips through a single class pair.

use std::future::Future;
use std::pin::pin;
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};
use std::task::{Context, Poll, RawWaker, RawWakerVTable, Waker};
use std::thread;
use std::time::Duration;

use serde::{Deserialize, Serialize};

use crate::bufferpool::{BulkRef, Error as PoolError, PageRef, PeerId, Req, StripeKey, Transport};
use crate::mercury::{
    BulkSource, Class, HgError, MercuryServer, MercuryTransport, PeerEntry, PeerRouter, StaticPeer,
    TransportConfig,
};
use crate::runtime::{DefaultRuntime, WorkerIdx};

// ---------------------------------------------------------------------------
// Tiny single-thread executor; same shape as bufferpool::tests but with a
// short park between polls so the Mercury progress thread can advance.
// ---------------------------------------------------------------------------

fn noop_waker() -> Waker {
    fn raw() -> RawWaker {
        RawWaker::new(std::ptr::null(), &VTABLE)
    }
    static VTABLE: RawWakerVTable = RawWakerVTable::new(|_| raw(), |_| {}, |_| {}, |_| {});
    // SAFETY: the vtable functions never dereference the data pointer.
    unsafe { Waker::from_raw(raw()) }
}

fn block_on<F: Future>(future: F) -> F::Output {
    block_on_within(future, 300_000)
}

/// Like `block_on` but with a caller-tunable spin cap. Each spin is
/// 100us, so `spins = 300_000` is ~30 s. Long-running tests use a
/// larger cap.
fn block_on_within<F: Future>(future: F, spin_cap: u64) -> F::Output {
    let waker = noop_waker();
    let mut cx = Context::from_waker(&waker);
    let mut fut = pin!(future);
    let mut spins: u64 = 0;
    loop {
        match fut.as_mut().poll(&mut cx) {
            Poll::Ready(v) => return v,
            Poll::Pending => {
                spins += 1;
                assert!(spins < spin_cap, "block_on stuck (no progress)");
                thread::sleep(Duration::from_micros(100));
            }
        }
    }
}

// ---------------------------------------------------------------------------
// Fake request and source.
// ---------------------------------------------------------------------------

#[derive(Clone, Serialize, Deserialize)]
struct TestReq {
    key: [u8; 32],
}

impl Req for TestReq {
    fn key(&self) -> StripeKey {
        StripeKey(self.key)
    }
}

/// Source that fills every reply with a fixed byte pattern. The
/// `calls` counter lets tests assert how many server fetches actually
/// fired.
struct EchoSource {
    pattern: u8,
    calls: Arc<AtomicUsize>,
}

impl BulkSource<TestReq> for EchoSource {
    async fn fetch(&self, _req: TestReq, _stripe_off: u64, len: u32) -> Result<Vec<u8>, HgError> {
        self.calls.fetch_add(1, Ordering::Relaxed);
        Ok(vec![self.pattern; len as usize])
    }
}

/// Source whose first `fail_count` invocations return a configurable
/// error and whose remaining invocations succeed with `pattern`.
struct FlakySource {
    pattern: u8,
    fail_code: i32,
    remaining_failures: AtomicUsize,
    calls: Arc<AtomicUsize>,
}

impl BulkSource<TestReq> for FlakySource {
    async fn fetch(&self, _req: TestReq, _stripe_off: u64, len: u32) -> Result<Vec<u8>, HgError> {
        self.calls.fetch_add(1, Ordering::Relaxed);
        let r = self.remaining_failures.load(Ordering::Relaxed);
        if r > 0 {
            self.remaining_failures.fetch_sub(1, Ordering::Relaxed);
            return Err(HgError::new(self.fail_code, "flaky-source"));
        }
        Ok(vec![self.pattern; len as usize])
    }
}

/// Source that returns fewer bytes than `len` so the server's short-
/// read guard fires.
struct ShortSource {
    short_by: u32,
    pattern: u8,
}

impl BulkSource<TestReq> for ShortSource {
    async fn fetch(&self, _req: TestReq, _stripe_off: u64, len: u32) -> Result<Vec<u8>, HgError> {
        let actual = (len - self.short_by) as usize;
        Ok(vec![self.pattern; actual])
    }
}

/// Source that returns *more* bytes than `len`. The server caps the
/// push at `len`; verify trailing bytes are not written to the
/// destination.
struct LongSource {
    over_by: u32,
    pattern: u8,
}

impl BulkSource<TestReq> for LongSource {
    async fn fetch(&self, _req: TestReq, _stripe_off: u64, len: u32) -> Result<Vec<u8>, HgError> {
        Ok(vec![self.pattern; (len + self.over_by) as usize])
    }
}

/// Source that resolves only when the caller flips `gate` true. The
/// "slow" surface lets us race teardown against an in-flight fetch.
struct GatedSource {
    pattern: u8,
    gate: Arc<AtomicBool>,
    calls: Arc<AtomicUsize>,
}

impl BulkSource<TestReq> for GatedSource {
    async fn fetch(&self, _req: TestReq, _stripe_off: u64, len: u32) -> Result<Vec<u8>, HgError> {
        self.calls.fetch_add(1, Ordering::Relaxed);
        // Spin yielding via thread::sleep is fine here: the server
        // task lives on a dedicated thread in these tests.
        while !self.gate.load(Ordering::Acquire) {
            thread::sleep(Duration::from_micros(200));
        }
        Ok(vec![self.pattern; len as usize])
    }
}

/// Source we plug in when a test does not care about the surface but
/// still needs a `BulkSource<TestReq>` type. Returns an error if ever
/// invoked.
struct NeverSource;

impl BulkSource<TestReq> for NeverSource {
    async fn fetch(&self, _req: TestReq, _stripe_off: u64, _len: u32) -> Result<Vec<u8>, HgError> {
        Err(HgError::new(-99, "NeverSource invoked"))
    }
}

// ---------------------------------------------------------------------------
// Test harness helpers.
// ---------------------------------------------------------------------------

const PAGE_SIZE: usize = 4096;

struct ServerHandle<B: BulkSource<TestReq> + Send + Sync + 'static> {
    class: Option<Class>,
    addr: String,
    thread: Option<thread::JoinHandle<()>>,
    _marker: std::marker::PhantomData<B>,
}

impl<B: BulkSource<TestReq> + Send + Sync + 'static> ServerHandle<B> {
    fn join_after_drop(mut self) {
        let class = self.class.take();
        drop(class);
        if let Some(t) = self.thread.take() {
            t.join().expect("server thread");
        }
    }

    fn addr(&self) -> &str {
        &self.addr
    }
}

impl<B: BulkSource<TestReq> + Send + Sync + 'static> Drop for ServerHandle<B> {
    fn drop(&mut self) {
        // Same teardown as `join_after_drop` but tolerated on the
        // failure path: drop the class first to close the queue,
        // then join the thread.
        let class = self.class.take();
        drop(class);
        if let Some(t) = self.thread.take() {
            let _ = t.join();
        }
    }
}

fn spawn_server<B>(source: B) -> ServerHandle<B>
where
    B: BulkSource<TestReq> + Send + Sync + 'static,
{
    let rt = DefaultRuntime::new(1);
    let cfg = TransportConfig {
        na_info: "na+sm://".into(),
        listen: true,
        peers: Vec::new(),
        max_inflight: 32,
        progress_poll_ms: 10,
        runtime: rt,
        worker_idx: WorkerIdx(0),
    };
    let class = Class::new(cfg).expect("server class");
    let addr = class.self_address().expect("self_address");
    let server = MercuryServer::<TestReq, _>::new(&class, source).expect("server attaches");
    let thread = thread::spawn(move || {
        block_on(server.run());
    });
    ServerHandle {
        class: Some(class),
        addr,
        thread: Some(thread),
        _marker: std::marker::PhantomData,
    }
}

/// Build a client `Class` whose peer table contains exactly one entry
/// for `peer_id` pointing at `addr`.
fn make_client(addr: &str, peer_id: PeerId) -> Class {
    let rt = DefaultRuntime::new(1);
    let cfg = TransportConfig {
        na_info: "na+sm://".into(),
        listen: false,
        peers: vec![PeerEntry {
            peer_id,
            addr: addr.to_string(),
        }],
        max_inflight: 16,
        progress_poll_ms: 10,
        runtime: rt,
        worker_idx: WorkerIdx(0),
    };
    Class::new(cfg).expect("client class")
}

/// Build the standard client used by happy-path and most error-path
/// tests: a `Class` with a single peer entry pointing at `server`,
/// a registered backing region, and a `MercuryTransport` wired to a
/// `StaticPeer(peer_id)` router. Tests that need a non-standard
/// `max_inflight`, a custom router, or a deliberately missing backing
/// continue to wire the pieces up by hand.
fn make_client_with_transport<B>(
    server: &ServerHandle<B>,
    peer_id: PeerId,
    backing: &mut [u8],
    page_size: usize,
) -> (Class, MercuryTransport<TestReq, StaticPeer>)
where
    B: BulkSource<TestReq> + Send + Sync + 'static,
{
    let class = make_client(server.addr(), peer_id);
    class
        .register_backing(backing.as_mut_ptr(), backing.len())
        .expect("register_backing");
    let transport = MercuryTransport::new(&class, StaticPeer(peer_id), page_size);
    (class, transport)
}

// ---------------------------------------------------------------------------
// 1. Happy path.
// ---------------------------------------------------------------------------

#[test]
fn round_trip_bulk_get_over_sm() {
    let calls = Arc::new(AtomicUsize::new(0));
    let server = spawn_server(EchoSource {
        pattern: 0xAB,
        calls: calls.clone(),
    });
    let mut backing = vec![0u8; PAGE_SIZE];
    let (client_class, transport) =
        make_client_with_transport(&server, PeerId(1), &mut backing, PAGE_SIZE);

    let req = TestReq { key: [0u8; 32] };
    let src = BulkRef {
        stripe: StripeKey([0u8; 32]),
        offset: 0,
        len: 64,
    };
    let dst = PageRef {
        page_idx: 0,
        offset: 0,
        len: 64,
    };
    block_on(transport.bulk_get(&req, src, dst)).expect("bulk_get");

    assert_eq!(&backing[..64], &[0xABu8; 64][..]);
    assert_eq!(&backing[64..PAGE_SIZE], &vec![0u8; PAGE_SIZE - 64][..]);
    assert_eq!(calls.load(Ordering::Relaxed), 1);

    drop(transport);
    drop(client_class);
    server.join_after_drop();
}

// ---------------------------------------------------------------------------
// 2. Routing / addressing / registration errors.
// ---------------------------------------------------------------------------

/// Router that always returns Err. Lets the test pin the
/// `PoolError::Transport` mapping without depending on
/// peer-table contents.
struct ErrRouter;
impl<R> PeerRouter<R> for ErrRouter {
    fn route(&self, _: &R) -> crate::mercury::Result<PeerId> {
        Err(HgError::new(-7, "router-says-no"))
    }
}

#[test]
fn router_error_surfaces_as_transport_error() {
    let server = spawn_server(EchoSource {
        pattern: 0xAA,
        calls: Arc::new(AtomicUsize::new(0)),
    });
    let client_class = make_client(server.addr(), PeerId(1));
    let mut backing = vec![0u8; PAGE_SIZE];
    client_class
        .register_backing(backing.as_mut_ptr(), PAGE_SIZE)
        .expect("register_backing");
    let transport: MercuryTransport<TestReq, _> =
        MercuryTransport::new(&client_class, ErrRouter, PAGE_SIZE);

    let req = TestReq { key: [0u8; 32] };
    let src = BulkRef {
        stripe: StripeKey([0u8; 32]),
        offset: 0,
        len: 8,
    };
    let dst = PageRef {
        page_idx: 0,
        offset: 0,
        len: 8,
    };
    match block_on(transport.bulk_get(&req, src, dst)) {
        Err(PoolError::Transport(_)) => {}
        other => panic!("expected Transport error, got {:?}", other),
    }
}

#[test]
fn unknown_peer_id_returns_transport_error() {
    let server = spawn_server(EchoSource {
        pattern: 0xAA,
        calls: Arc::new(AtomicUsize::new(0)),
    });
    let client_class = make_client(server.addr(), PeerId(1));
    let mut backing = vec![0u8; PAGE_SIZE];
    client_class
        .register_backing(backing.as_mut_ptr(), PAGE_SIZE)
        .expect("register_backing");
    // Router points at a peer not present in the table.
    let transport: MercuryTransport<TestReq, _> =
        MercuryTransport::new(&client_class, StaticPeer(PeerId(42)), PAGE_SIZE);
    let req = TestReq { key: [0u8; 32] };
    let src = BulkRef {
        stripe: StripeKey([0u8; 32]),
        offset: 0,
        len: 8,
    };
    let dst = PageRef {
        page_idx: 0,
        offset: 0,
        len: 8,
    };
    match block_on(transport.bulk_get(&req, src, dst)) {
        Err(PoolError::Transport(_)) => {}
        other => panic!("expected Transport error, got {:?}", other),
    }
}

#[test]
fn offset_overflow_returns_out_of_range() {
    // Build a client whose transport's page_size, combined with a
    // u32::MAX page_idx, overflows u64. We do not register any
    // backing because `bulk_get`'s overflow guard runs before the
    // backing lookup; the router/class still need to be live.
    let server = spawn_server(EchoSource {
        pattern: 0xAA,
        calls: Arc::new(AtomicUsize::new(0)),
    });
    let client_class = make_client(server.addr(), PeerId(1));
    // 2^33 byte pages: u32::MAX * 2^33 > u64::MAX.
    let huge_page = 1usize << 33;
    let transport: MercuryTransport<TestReq, _> =
        MercuryTransport::new(&client_class, StaticPeer(PeerId(1)), huge_page);

    let req = TestReq { key: [0u8; 32] };
    let src = BulkRef {
        stripe: StripeKey([0u8; 32]),
        offset: 0,
        len: 8,
    };
    let dst = PageRef {
        page_idx: u32::MAX,
        offset: 0,
        len: 8,
    };
    match block_on(transport.bulk_get(&req, src, dst)) {
        Err(PoolError::OffsetOutOfRange) => {}
        other => panic!("expected OffsetOutOfRange, got {:?}", other),
    }
}

#[test]
fn backing_not_registered_returns_transport_error() {
    let server = spawn_server(EchoSource {
        pattern: 0xAA,
        calls: Arc::new(AtomicUsize::new(0)),
    });
    let client_class = make_client(server.addr(), PeerId(1));
    // Deliberately skip `register_backing`.
    let transport: MercuryTransport<TestReq, _> =
        MercuryTransport::new(&client_class, StaticPeer(PeerId(1)), PAGE_SIZE);
    let req = TestReq { key: [0u8; 32] };
    let src = BulkRef {
        stripe: StripeKey([0u8; 32]),
        offset: 0,
        len: 8,
    };
    let dst = PageRef {
        page_idx: 0,
        offset: 0,
        len: 8,
    };
    match block_on(transport.bulk_get(&req, src, dst)) {
        Err(PoolError::Transport(_)) => {}
        other => panic!("expected Transport error, got {:?}", other),
    }
}

#[test]
fn register_backing_idempotent_and_rejects_other_region() {
    let rt = DefaultRuntime::new(1);
    let cfg = TransportConfig {
        na_info: "na+sm://".into(),
        listen: false,
        peers: Vec::new(),
        max_inflight: 1,
        progress_poll_ms: 10,
        runtime: rt,
        worker_idx: WorkerIdx(0),
    };
    let class = Class::new(cfg).expect("class");
    let mut a = vec![0u8; PAGE_SIZE];
    let mut b = vec![0u8; PAGE_SIZE];

    class
        .register_backing(a.as_mut_ptr(), PAGE_SIZE)
        .expect("first register_backing");
    // Idempotent for the same (base, size).
    class
        .register_backing(a.as_mut_ptr(), PAGE_SIZE)
        .expect("idempotent");
    // Different region rejected.
    assert!(class.register_backing(b.as_mut_ptr(), PAGE_SIZE).is_err());
    // Different size, same base rejected.
    assert!(
        class
            .register_backing(a.as_mut_ptr(), PAGE_SIZE * 2)
            .is_err()
    );
}

#[test]
fn peer_table_partial_failure_aborts_class_construction() {
    // The second entry has an embedded NUL, which fails the
    // `CString::new` guard inside `PeerTable::new`; the first
    // (well-formed) address must have its `hg_addr_t` freed via
    // `PeerTable::drop` even though the table itself is not exposed.
    // We cannot directly observe the free, but `Class::new` returning
    // `Err` and the test process not leaking is the public guarantee
    // and is what we pin here.
    let server = spawn_server(EchoSource {
        pattern: 0xAA,
        calls: Arc::new(AtomicUsize::new(0)),
    });
    let rt = DefaultRuntime::new(1);
    let cfg = TransportConfig {
        na_info: "na+sm://".into(),
        listen: false,
        peers: vec![
            PeerEntry {
                peer_id: PeerId(1),
                addr: server.addr().to_string(),
            },
            PeerEntry {
                peer_id: PeerId(2),
                addr: "na+sm://has-nul\0/x".to_string(),
            },
        ],
        max_inflight: 1,
        progress_poll_ms: 10,
        runtime: rt,
        worker_idx: WorkerIdx(0),
    };
    assert!(Class::new(cfg).is_err());
}

#[test]
fn unresolvable_peer_aborts_class_construction() {
    let rt = DefaultRuntime::new(1);
    let cfg = TransportConfig {
        na_info: "na+sm://".into(),
        listen: false,
        peers: vec![PeerEntry {
            peer_id: PeerId(1),
            // Syntactically valid Mercury address that points at
            // nothing. `HG_Addr_lookup2` against SM with a malformed
            // tail typically returns non-zero.
            addr: "na+sm://does-not-parse".to_string(),
        }],
        max_inflight: 1,
        progress_poll_ms: 10,
        runtime: rt,
        worker_idx: WorkerIdx(0),
    };
    assert!(Class::new(cfg).is_err());
}

#[test]
fn self_address_round_trips_to_valid_utf8() {
    let server = spawn_server(EchoSource {
        pattern: 0xAA,
        calls: Arc::new(AtomicUsize::new(0)),
    });
    let addr = server.addr();
    assert!(!addr.is_empty());
    assert!(!addr.contains('\0'), "trailing NUL not stripped: {addr:?}");
    assert!(addr.starts_with("na+sm://"), "unexpected: {addr:?}");
}

#[test]
fn client_only_class_has_no_server_queue() {
    let rt = DefaultRuntime::new(1);
    let cfg = TransportConfig {
        na_info: "na+sm://".into(),
        listen: false,
        peers: Vec::new(),
        max_inflight: 1,
        progress_poll_ms: 10,
        runtime: rt,
        worker_idx: WorkerIdx(0),
    };
    let class = Class::new(cfg).expect("class");
    assert!(class.server_queue().is_none());
    // `MercuryServer::new` against a client-only class returns None.
    let s: Option<MercuryServer<TestReq, NeverSource>> = MercuryServer::new(&class, NeverSource);
    assert!(s.is_none());
}

// ---------------------------------------------------------------------------
// 3. Server-side error propagation.
// ---------------------------------------------------------------------------

#[test]
fn server_fetch_error_propagates_to_client_status() {
    let calls = Arc::new(AtomicUsize::new(0));
    let source = FlakySource {
        pattern: 0xCC,
        fail_code: 17,
        remaining_failures: AtomicUsize::new(1),
        calls: calls.clone(),
    };
    let server = spawn_server(source);
    let mut backing = vec![0u8; PAGE_SIZE];
    let (_client_class, transport) =
        make_client_with_transport(&server, PeerId(1), &mut backing, PAGE_SIZE);

    let req = TestReq { key: [0u8; 32] };
    let src = BulkRef {
        stripe: StripeKey([0u8; 32]),
        offset: 0,
        len: 32,
    };
    let dst = PageRef {
        page_idx: 0,
        offset: 0,
        len: 32,
    };
    // First call must fail with the server's status code (17).
    let err =
        block_on(transport.bulk_get(&req, src, dst)).expect_err("expected server-status error");
    match err {
        PoolError::Transport(e) => {
            let msg = format!("{e}");
            assert!(msg.contains("17"), "expected status 17 in {msg:?}");
        }
        _ => panic!("unexpected error variant"),
    }
    // Destination must be untouched on error.
    assert!(backing.iter().take(32).all(|&b| b == 0));

    // Second call succeeds and writes the pattern.
    let src = BulkRef {
        stripe: StripeKey([0u8; 32]),
        offset: 0,
        len: 32,
    };
    let dst = PageRef {
        page_idx: 0,
        offset: 0,
        len: 32,
    };
    block_on(transport.bulk_get(&req, src, dst)).expect("retry must succeed");
    assert_eq!(&backing[..32], &[0xCC; 32]);
    assert_eq!(calls.load(Ordering::Relaxed), 2);
}

#[test]
fn server_short_read_propagates_error() {
    let server = spawn_server(ShortSource {
        short_by: 4,
        pattern: 0x55,
    });
    let mut backing = vec![0u8; PAGE_SIZE];
    let (_client_class, transport) =
        make_client_with_transport(&server, PeerId(1), &mut backing, PAGE_SIZE);
    let req = TestReq { key: [0u8; 32] };
    let src = BulkRef {
        stripe: StripeKey([0u8; 32]),
        offset: 0,
        len: 16,
    };
    let dst = PageRef {
        page_idx: 0,
        offset: 0,
        len: 16,
    };
    assert!(matches!(
        block_on(transport.bulk_get(&req, src, dst)),
        Err(PoolError::Transport(_))
    ));
    // No partial write: destination stays zero.
    assert!(backing.iter().take(16).all(|&b| b == 0));
}

#[test]
fn server_over_production_is_capped_at_len() {
    let server = spawn_server(LongSource {
        over_by: 32,
        pattern: 0x77,
    });
    let mut backing = vec![0u8; PAGE_SIZE];
    let (_client_class, transport) =
        make_client_with_transport(&server, PeerId(1), &mut backing, PAGE_SIZE);
    let req = TestReq { key: [0u8; 32] };
    let len = 16u32;
    let src = BulkRef {
        stripe: StripeKey([0u8; 32]),
        offset: 0,
        len,
    };
    let dst = PageRef {
        page_idx: 0,
        offset: 0,
        len,
    };
    block_on(transport.bulk_get(&req, src, dst)).expect("over-production must still succeed");
    // Exactly `len` bytes were written.
    assert_eq!(&backing[..len as usize], &vec![0x77; len as usize][..]);
    assert!(backing[len as usize..].iter().all(|&b| b == 0));
}

// ---------------------------------------------------------------------------
// 4. Geometry: non-zero page_idx and offset.
// ---------------------------------------------------------------------------

#[test]
fn page_offset_math_writes_to_correct_byte_range() {
    let server = spawn_server(EchoSource {
        pattern: 0xEE,
        calls: Arc::new(AtomicUsize::new(0)),
    });
    // Multi-page backing so we can target page_idx = 2 specifically.
    const PAGES: usize = 4;
    let mut backing = vec![0u8; PAGE_SIZE * PAGES];
    let (_client_class, transport) =
        make_client_with_transport(&server, PeerId(1), &mut backing, PAGE_SIZE);
    let req = TestReq { key: [0u8; 32] };
    let len = 128u32;
    let page_idx = 2u32;
    let offset = 256u32;
    let src = BulkRef {
        stripe: StripeKey([0u8; 32]),
        offset: 0,
        len,
    };
    let dst = PageRef {
        page_idx,
        offset,
        len,
    };
    block_on(transport.bulk_get(&req, src, dst)).expect("bulk_get");

    let start = page_idx as usize * PAGE_SIZE + offset as usize;
    let end = start + len as usize;
    // The targeted slice is the pattern; everything else stays zero.
    for (i, &b) in backing.iter().enumerate() {
        if (start..end).contains(&i) {
            assert_eq!(b, 0xEE, "expected 0xEE at byte {i}");
        } else {
            assert_eq!(b, 0, "expected 0 at byte {i}, got {b:#x}");
        }
    }
}

// ---------------------------------------------------------------------------
// 5. Concurrency: many in-flight, registry full.
// ---------------------------------------------------------------------------

#[test]
fn many_concurrent_bulk_gets_complete() {
    let calls = Arc::new(AtomicUsize::new(0));
    let server = spawn_server(EchoSource {
        pattern: 0x33,
        calls: calls.clone(),
    });
    const N: usize = 16;
    let mut backing = vec![0u8; PAGE_SIZE * N];
    let (_client_class, transport) =
        make_client_with_transport(&server, PeerId(1), &mut backing, PAGE_SIZE);

    let req = TestReq { key: [0u8; 32] };

    // Issue them concurrently by polling many futures inside a single
    // executor task. We round-robin a Vec of pinned futures until all
    // complete; this stresses the completion registry against a real
    // Mercury class.
    block_on(async {
        use std::pin::Pin;
        let mut futs: Vec<Pin<Box<dyn Future<Output = Result<(), PoolError>>>>> = Vec::new();
        for i in 0..N {
            let src = BulkRef {
                stripe: StripeKey([0u8; 32]),
                offset: 0,
                len: 64,
            };
            let dst = PageRef {
                page_idx: i as u32,
                offset: 0,
                len: 64,
            };
            futs.push(Box::pin(transport.bulk_get(&req, src, dst)));
        }
        // Manually drive the futures until all are ready.
        let waker = noop_waker();
        let mut cx = Context::from_waker(&waker);
        let mut done = vec![false; N];
        let mut remaining = N;
        let mut spins: u64 = 0;
        while remaining > 0 {
            spins += 1;
            assert!(spins < 600_000, "concurrent stuck");
            for (i, fut) in futs.iter_mut().enumerate() {
                if done[i] {
                    continue;
                }
                if let Poll::Ready(r) = fut.as_mut().poll(&mut cx) {
                    r.expect("concurrent bulk_get");
                    done[i] = true;
                    remaining -= 1;
                }
            }
            if remaining > 0 {
                thread::sleep(Duration::from_micros(100));
            }
        }
    });

    for i in 0..N {
        let start = i * PAGE_SIZE;
        assert_eq!(&backing[start..start + 64], &[0x33; 64][..], "page {i}");
    }
    assert_eq!(calls.load(Ordering::Relaxed), N);
}

#[test]
fn registry_full_rejects_overflow_submission() {
    // Build a client whose max_inflight is 1, then keep one forward
    // gated by a slow server source so the second submission has
    // nowhere to go.
    let gate = Arc::new(AtomicBool::new(false));
    let calls = Arc::new(AtomicUsize::new(0));
    let server = spawn_server(GatedSource {
        pattern: 0x44,
        gate: gate.clone(),
        calls: calls.clone(),
    });

    let rt = DefaultRuntime::new(1);
    let cfg = TransportConfig {
        na_info: "na+sm://".into(),
        listen: false,
        peers: vec![PeerEntry {
            peer_id: PeerId(1),
            addr: server.addr().to_string(),
        }],
        max_inflight: 1,
        progress_poll_ms: 10,
        runtime: rt,
        worker_idx: WorkerIdx(0),
    };
    let client_class = Class::new(cfg).expect("client class");

    let mut backing = vec![0u8; PAGE_SIZE * 2];
    client_class
        .register_backing(backing.as_mut_ptr(), PAGE_SIZE * 2)
        .expect("register_backing");
    let transport: MercuryTransport<TestReq, _> =
        MercuryTransport::new(&client_class, StaticPeer(PeerId(1)), PAGE_SIZE);
    let req = TestReq { key: [0u8; 32] };

    let first_src = BulkRef {
        stripe: StripeKey([0u8; 32]),
        offset: 0,
        len: 16,
    };
    let first_dst = PageRef {
        page_idx: 0,
        offset: 0,
        len: 16,
    };
    let second_src = BulkRef {
        stripe: StripeKey([0u8; 32]),
        offset: 0,
        len: 16,
    };
    let second_dst = PageRef {
        page_idx: 1,
        offset: 0,
        len: 16,
    };

    block_on(async {
        let mut first = Box::pin(transport.bulk_get(&req, first_src, first_dst));
        let waker = noop_waker();
        let mut cx = Context::from_waker(&waker);
        // Poll the first one once to allocate its slot and submit
        // the forward; the server is gated so it will not complete.
        let _ = first.as_mut().poll(&mut cx);

        // Spin until the server records that it has received the
        // forward and is waiting on the gate. Otherwise the first
        // bulk_get may not yet have allocated its slot.
        for _ in 0..30_000 {
            if calls.load(Ordering::Relaxed) >= 1 {
                break;
            }
            thread::sleep(Duration::from_micros(100));
        }

        // Second submission must fail fast.
        let r = transport.bulk_get(&req, second_src, second_dst).await;
        assert!(matches!(r, Err(PoolError::Transport(_))));

        // Release the gate so the first call completes; otherwise
        // teardown waits forever for the in-flight RPC.
        gate.store(true, Ordering::Release);
        first.await.expect("first bulk_get completes once gated");
    });
}

// ---------------------------------------------------------------------------
// 6. Teardown.
// ---------------------------------------------------------------------------

#[test]
fn server_run_exits_when_class_drops() {
    let server = spawn_server(EchoSource {
        pattern: 0xAA,
        calls: Arc::new(AtomicUsize::new(0)),
    });
    // Tear down with no traffic: dropping the class closes the
    // server queue, the server task observes `Ready(None)`, and the
    // worker thread joins.
    server.join_after_drop();
}

#[test]
fn teardown_with_class_dropped_before_transport() {
    let server = spawn_server(EchoSource {
        pattern: 0xBE,
        calls: Arc::new(AtomicUsize::new(0)),
    });
    let mut backing = vec![0u8; PAGE_SIZE];
    let (client_class, transport) =
        make_client_with_transport(&server, PeerId(1), &mut backing, PAGE_SIZE);
    let req = TestReq { key: [0u8; 32] };
    let src = BulkRef {
        stripe: StripeKey([0u8; 32]),
        offset: 0,
        len: 8,
    };
    let dst = PageRef {
        page_idx: 0,
        offset: 0,
        len: 8,
    };
    block_on(transport.bulk_get(&req, src, dst)).expect("bulk_get");

    // Reverse order vs. the happy-path test: drop class first.
    // Transport holds an Arc to ClassInner, so the FFI teardown
    // defers until the transport drops too.
    drop(client_class);
    drop(transport);
    server.join_after_drop();
}

// ---------------------------------------------------------------------------
// 7. Construction-time guards (panics).
// ---------------------------------------------------------------------------

#[test]
#[should_panic(expected = "page_size")]
fn transport_new_panics_on_zero_page_size() {
    let rt = DefaultRuntime::new(1);
    let cfg = TransportConfig {
        na_info: "na+sm://".into(),
        listen: false,
        peers: Vec::new(),
        max_inflight: 1,
        progress_poll_ms: 10,
        runtime: rt,
        worker_idx: WorkerIdx(0),
    };
    let class = Class::new(cfg).expect("class");
    let _t: MercuryTransport<TestReq, _> = MercuryTransport::new(&class, StaticPeer(PeerId(1)), 0);
}

#[test]
#[should_panic(expected = "page_size")]
fn transport_new_panics_on_non_power_of_two_page_size() {
    let rt = DefaultRuntime::new(1);
    let cfg = TransportConfig {
        na_info: "na+sm://".into(),
        listen: false,
        peers: Vec::new(),
        max_inflight: 1,
        progress_poll_ms: 10,
        runtime: rt,
        worker_idx: WorkerIdx(0),
    };
    let class = Class::new(cfg).expect("class");
    let _t: MercuryTransport<TestReq, _> =
        MercuryTransport::new(&class, StaticPeer(PeerId(1)), 3000);
}

// ---------------------------------------------------------------------------
// 8. Stress.
// ---------------------------------------------------------------------------

/// 5,000 back-to-back transfers through a single class pair. Run as
/// part of the regular test suite to keep the round-trip path
/// continuously exercised under load; the SM backend finishes this in
/// well under a second on developer hardware.
#[test]
fn stress_round_trip_5k() {
    let calls = Arc::new(AtomicUsize::new(0));
    let server = spawn_server(EchoSource {
        pattern: 0x99,
        calls: calls.clone(),
    });
    let mut backing = vec![0u8; PAGE_SIZE];
    let (client_class, transport) =
        make_client_with_transport(&server, PeerId(1), &mut backing, PAGE_SIZE);

    let req = TestReq { key: [0u8; 32] };
    for _ in 0..5_000 {
        let src = BulkRef {
            stripe: StripeKey([0u8; 32]),
            offset: 0,
            len: 64,
        };
        let dst = PageRef {
            page_idx: 0,
            offset: 0,
            len: 64,
        };
        block_on_within(transport.bulk_get(&req, src, dst), 1_000_000).expect("stress bulk_get");
    }
    assert_eq!(calls.load(Ordering::Relaxed), 5_000);
    assert_eq!(&backing[..64], &[0x99; 64]);

    drop(transport);
    drop(client_class);
    server.join_after_drop();
}
