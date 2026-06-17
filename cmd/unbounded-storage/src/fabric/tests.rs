// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Module integration tests for the fabric module. Covers connection
//! CRUD, MR registration, and the streaming RPC path over a loopback
//! tcp fabric. Each test skips during setup if explicitly requested or
//! if the tcp provider is not installed in the libfabric build
//! available to the test binary.

use std::ffi::CString;
use std::ptr;
use std::sync::Arc;
use std::time::Duration;

use crate::fabric::PeerId;
use crate::fabric::{ConnectionSpec, Fabric, FabricConfig, FabricError, Provider, defaults_for};
use crate::runtime::{DefaultRuntime, Threading, WorkerIdx};

use super::ffi;

/// Skip-gate: returns true if libfabric reports a usable `tcp`
/// provider in this environment.
fn tcp_provider_available() -> bool {
    let prov = CString::new("tcp").unwrap();
    // SAFETY: prov outlives the call; out-param is a stack pointer.
    let hints = unsafe { ffi::ub_fi_build_hints(prov.as_ptr()) };
    if hints.is_null() {
        return false;
    }
    let mut info: *mut ffi::fi_info = ptr::null_mut();
    let rc = unsafe {
        ffi::fi_getinfo(
            ffi::requested_version(),
            ptr::null(),
            ptr::null(),
            0,
            hints,
            &mut info,
        )
    };
    if !info.is_null() {
        unsafe { ffi::fi_freeinfo(info) };
    }
    unsafe { ffi::fi_freeinfo(hints) };
    rc == 0
}

fn skip_ffi() -> bool {
    if std::env::var_os("FABRIC_SKIP_FFI").is_some() {
        eprintln!("FABRIC_SKIP_FFI set; skipping fabric ffi test");
        return true;
    }
    if !tcp_provider_available() {
        eprintln!("libfabric tcp provider unavailable; skipping fabric ffi test");
        return true;
    }
    false
}

fn rt() -> Arc<dyn Threading> {
    DefaultRuntime::new(1)
}

fn tcp_loopback_cfg() -> FabricConfig {
    let mut cfg = defaults_for("lo", rt(), WorkerIdx(0));
    cfg.provider = Provider::Tcp;
    cfg.progress_threads = 1;
    cfg.max_inflight = 64;
    // Pin the source address to loopback. Without this, fi_getinfo with
    // a null node lets libfabric pick the first routable NIC; on
    // multi-NIC or containerized hosts the paired fabrics land on
    // different interfaces and the tcp RDM data path never makes
    // progress, so every completion-dependent test times out. The ":0"
    // port lets the OS assign a free ephemeral port per fabric;
    // listen=true is required so fi_getinfo treats the address as a
    // source bind (FI_SOURCE) rather than a destination.
    cfg.listen = true;
    cfg.listen_addr = Some("127.0.0.1:0".to_string());
    cfg
}

fn new_tcp_fabric() -> Fabric {
    Fabric::new(tcp_loopback_cfg())
        .expect("Fabric::new tcp failed after provider availability gate")
}

/// Build a loopback fabric with an explicit `self_peer`. Establishment
/// is single-dialer (the lower-id node dials), so paired fabrics must
/// carry globally-consistent, distinct ids for exactly one side to dial.
fn new_tcp_fabric_with_peer(self_peer: PeerId) -> Fabric {
    let mut cfg = tcp_loopback_cfg();
    cfg.self_peer = self_peer;
    Fabric::new(cfg).expect("Fabric::new tcp failed after provider availability gate")
}

/// Derive a `wire_addr` for `peer_fabric` understood by this build's
/// provider. Under FI_EP_MSG `self_address` already returns the
/// listener's "ip:port" string, which is exactly the dial target.
fn wire_addr_of(peer_fabric: &Fabric) -> String {
    let addr = peer_fabric.self_address().expect("self_address");
    assert!(!addr.is_empty(), "empty self-address");
    addr
}

#[test]
fn add_remove_add_cycle() {
    if skip_ffi() {
        return;
    }
    let f = new_tcp_fabric();
    let peer = new_tcp_fabric();
    let addr = wire_addr_of(&peer);
    let spec = ConnectionSpec {
        peer: PeerId(42),
        wire_addr: addr.clone(),
        hca_numa: None,
        labels: Vec::new(),
    };
    f.add_connection(spec.clone()).expect("add 1");
    assert!(f.list_connections().contains(&PeerId(42)));
    f.remove_connection(PeerId(42)).expect("remove");
    assert!(!f.list_connections().contains(&PeerId(42)));
    f.add_connection(spec).expect("add 2");
    assert!(f.list_connections().contains(&PeerId(42)));
}

/// The background reconnect thread must dial a peer that was only ever
/// recorded as desired (never dialed via `add_connection`). This is the
/// startup-race gap: if both directed dials fail, the pair still
/// converges once a listener is reachable. We seed the desired set
/// directly with `set_desired_peers` (bypassing the immediate dial in
/// `add_connection`) and assert the reconnect loop establishes the
/// connection on its own within a few intervals.
#[test]
fn background_reconnect_dials_desired_peer() {
    if skip_ffi() {
        return;
    }
    let f = new_tcp_fabric();
    let peer = new_tcp_fabric();
    let spec = ConnectionSpec {
        peer: PeerId(99),
        wire_addr: wire_addr_of(&peer),
        hca_numa: None,
        labels: Vec::new(),
    };
    // Record intent without dialing; only the background thread can
    // establish this connection.
    f.set_desired_peers(vec![spec]);
    assert!(
        !f.list_connections().contains(&PeerId(99)),
        "connection must not exist before the reconnect thread runs"
    );

    // RECONNECT_INTERVAL_MS is 1s; allow several intervals before
    // giving up so a slow CI box does not flake.
    let deadline = std::time::Instant::now() + Duration::from_secs(10);
    let mut connected = false;
    while std::time::Instant::now() < deadline {
        if f.list_connections().contains(&PeerId(99)) {
            connected = true;
            break;
        }
        std::thread::sleep(Duration::from_millis(100));
    }
    assert!(
        connected,
        "background reconnect thread never established the desired connection"
    );
}

#[test]
fn numa_mismatch_rejects() {
    if skip_ffi() {
        return;
    }
    let mut cfg = tcp_loopback_cfg();
    cfg.numa = Some(0);
    let f = Fabric::new(cfg).expect("Fabric::new failed after provider availability gate");
    let spec = ConnectionSpec {
        peer: PeerId(7),
        wire_addr: "127.0.0.1:1".to_string(),
        hca_numa: Some(1),
        labels: Vec::new(),
    };
    match f.add_connection(spec) {
        Err(FabricError::NumaMismatch { expected, got }) => {
            assert_eq!(expected, 0);
            assert_eq!(got, 1);
        }
        other => panic!("expected NumaMismatch, got {other:?}"),
    }
}

#[test]
fn register_backing_numa_mismatch() {
    if skip_ffi() {
        return;
    }
    let mut cfg = tcp_loopback_cfg();
    cfg.numa = Some(0);
    let f = Fabric::new(cfg).expect("Fabric::new failed after provider availability gate");
    let backing = crate::memory::allocate(crate::memory::BackingRequest {
        kind: crate::memory::BackingKind::Heap,
        bytes: crate::memory::HUGEPAGE_2MB,
        numa: None,
    })
    .expect("heap backing");
    match f.register_backing(&backing, Some(1)) {
        Err(FabricError::NumaMismatch { expected, got }) => {
            assert_eq!(expected, 0);
            assert_eq!(got, 1);
        }
        other => panic!("expected NumaMismatch, got {other:?}"),
    }
}

// ---------------------------------------------------------------
// Phase 5a: streaming RPC tests.
//
// Each test wires two tcp-loopback fabrics, registers a heap-backed
// `Backing` on each side, exchanges connections, starts an RPC
// server on the server fabric with a canned handler, and drives the
// client `FabricTransport::bulk_get` against it. All tests
// runtime-skip if the libfabric tcp provider is unavailable.
// ---------------------------------------------------------------

use serde::{Deserialize, Serialize};
use std::pin::Pin;
use std::sync::Mutex;
use std::sync::atomic::{AtomicBool, Ordering};
use std::task::{Context, Poll};

use crate::bufferpool::{BulkRef, PageRef, PageStream, Req, StripeKey, Transport};
use crate::fabric::handler::{CancelObservingHandler, ErrorHandler, NPagesHandler, TestErr};
use crate::fabric::{FabricTransport, Handler, HandlerStream, MrHandle, PoolHandler, StaticPeer};

#[derive(Clone, Serialize, Deserialize, Debug)]
struct TestReq {
    tag: u64,
}

impl Req for TestReq {
    fn key(&self) -> StripeKey {
        // Derive a per-request stripe key from the tag so that a
        // request's advertised key (what a `BlockStore`-backed handler
        // reads) is unique and matches what the test populated. In
        // production `Req::key()` is the real stripe key; a constant
        // here would make every request collide on one store entry.
        StripeKey(stripe_for_tag(self.tag))
    }
}

/// Map a `TestReq` tag to a deterministic 32-byte stripe key by
/// writing the tag into the leading bytes. Used so tests can
/// populate a store under exactly the key a given request advertises.
fn stripe_for_tag(tag: u64) -> [u8; 32] {
    let mut k = [0u8; 32];
    k[..8].copy_from_slice(&tag.to_le_bytes());
    k
}

struct RecordingHandler {
    seen: Arc<Mutex<Option<BulkRef>>>,
}

struct EmptyStream;

impl HandlerStream for EmptyStream {
    type Error = std::convert::Infallible;

    fn poll_next(
        self: Pin<&mut Self>,
        _cx: &mut Context<'_>,
    ) -> Poll<Option<Result<PageRef, Self::Error>>> {
        Poll::Ready(None)
    }
}

impl<R: Req> Handler<R> for RecordingHandler {
    type Error = std::convert::Infallible;
    type Stream<'a>
        = EmptyStream
    where
        Self: 'a,
        R: 'a;

    fn handle<'a>(&'a self, _req: &'a R, src: BulkRef, _hops_remaining: u32) -> Self::Stream<'a> {
        *self.seen.lock().expect("recording handler lock poisoned") = Some(src);
        EmptyStream
    }
}

/// Bring up a paired (server, client) pair of fabrics over tcp
/// loopback. Returns (server, client, server_local_mr,
/// client_local_mr, n_pages, page_size).
fn paired_fabrics(n_pages: usize) -> (Arc<Fabric>, Arc<Fabric>, MrHandle, MrHandle, usize, usize) {
    // server is node 2, client is node 1: single-dialer means the
    // client (lower id) dials the server, which accepts.
    let server = Arc::new(new_tcp_fabric_with_peer(PeerId(2)));
    let client = Arc::new(new_tcp_fabric_with_peer(PeerId(1)));

    let page_size = crate::memory::HUGEPAGE_2MB;
    let bytes = page_size * n_pages;
    let server_backing = crate::memory::allocate(crate::memory::BackingRequest {
        kind: crate::memory::BackingKind::Heap,
        bytes,
        numa: None,
    })
    .expect("server heap backing allocation failed");
    let client_backing = crate::memory::allocate(crate::memory::BackingRequest {
        kind: crate::memory::BackingKind::Heap,
        bytes,
        numa: None,
    })
    .expect("client heap backing allocation failed");

    // Synthetic content on the server side: page i filled with byte i.
    // SAFETY: server_backing.base is non-null and covers `bytes`.
    unsafe {
        for i in 0..n_pages {
            let p = server_backing.base.add(i * page_size);
            std::ptr::write_bytes(p, (i as u8).wrapping_add(0x11), page_size);
        }
    }
    // Client side: zero out so we can detect bytes that came in.
    // SAFETY: client_backing.base covers `bytes`.
    unsafe {
        std::ptr::write_bytes(client_backing.base, 0, bytes);
    }

    let server_mr = server
        .register_backing(&server_backing, None)
        .expect("server MR registration failed after provider availability gate");
    let client_mr = client
        .register_backing(&client_backing, None)
        .expect("client MR registration failed after provider availability gate");

    let server_addr = wire_addr_of(&server);
    let client_addr = wire_addr_of(&client);
    server
        .add_connection(ConnectionSpec {
            peer: PeerId(1), // client peer-id in server's table
            wire_addr: client_addr,
            hca_numa: None,
            labels: Vec::new(),
        })
        .expect("server.add_connection failed after provider availability gate");
    client
        .add_connection(ConnectionSpec {
            peer: PeerId(2), // server peer-id in client's table
            wire_addr: server_addr,
            hca_numa: None,
            labels: Vec::new(),
        })
        .expect("client.add_connection failed after provider availability gate");

    // Leak the backings so they outlive the test - their drop carriers
    // would deallocate before the fabrics finish using them otherwise.
    // The process exits at the end of the test binary so this is not
    // a real leak in practice.
    Box::leak(Box::new(server_backing));
    Box::leak(Box::new(client_backing));

    (server, client, server_mr, client_mr, n_pages, page_size)
}

/// Block-on a `PageStream` until it yields `None`, collecting all
/// emitted page results. Bounded wallclock to avoid hangs.
fn drain_stream<'a, S: PageStream + ?Sized>(
    mut stream: std::pin::Pin<&mut S>,
    timeout: Duration,
) -> Vec<Result<PageRef, crate::bufferpool::Error>> {
    use std::task::{Context, Poll};

    let waker = crate::runtime::noop_waker();
    let mut cx = Context::from_waker(&waker);

    let started = std::time::Instant::now();
    let mut out = Vec::new();
    loop {
        match stream.as_mut().poll_next(&mut cx) {
            Poll::Ready(None) => return out,
            Poll::Ready(Some(item)) => out.push(item),
            Poll::Pending => {
                if started.elapsed() >= timeout {
                    panic!("drain_stream timed out after {:?}", started.elapsed());
                }
                std::thread::sleep(Duration::from_millis(5));
            }
        }
    }
}

#[test]
fn rpc_success_path_n_pages() {
    if skip_ffi() {
        return;
    }
    let (server, client, server_mr, client_mr, n_pages, page_size) = paired_fabrics(8);

    // Server-side handler emits one PageRef per page.
    let pages: Vec<PageRef> = (0..n_pages)
        .map(|i| PageRef {
            page_idx: i as u32,
            offset: 0,
            len: page_size as u32,
        })
        .collect();
    let handler = Arc::new(NPagesHandler {
        pages: pages.clone(),
    });
    let _server_handle = Fabric::start_rpc_server::<TestReq, _>(
        &server,
        handler,
        Some(server_mr),
        crate::memory::HUGEPAGE_2MB,
    )
    .expect("start_rpc_server failed after provider availability gate");

    let transport = FabricTransport::<TestReq, _>::new(
        client.clone(),
        client_mr,
        StaticPeer { peer: PeerId(2) },
        page_size,
    )
    .expect("FabricTransport::new failed after provider availability gate");

    let req = TestReq { tag: 0xCAFE };
    let dsts: Vec<PageRef> = (0..n_pages)
        .map(|i| PageRef {
            page_idx: i as u32,
            offset: 0,
            len: page_size as u32,
        })
        .collect();
    let src = BulkRef {
        stripe: StripeKey([0u8; 32]),
        offset: 0,
        len: page_size as u32,
    };
    let mut stream = transport.bulk_get(&req, src, &dsts);
    // SAFETY: stream is pinned on the stack and never moved.
    let stream = unsafe { std::pin::Pin::new_unchecked(&mut stream) };
    let results = drain_stream(stream, Duration::from_secs(20));

    assert_eq!(
        results.len(),
        n_pages,
        "rpc_success_path_n_pages returned the wrong result count",
    );
    for r in &results {
        assert!(r.is_ok(), "unexpected stream error: {r:?}");
    }

    // Content check: client backing page i should now contain the
    // server's synthetic byte for that page.
    // SAFETY: client_mr.base is the live mapping; n_pages * page_size
    // is within bounds.
    unsafe {
        for i in 0..n_pages {
            let p = client_mr.base as *const u8;
            let byte = *p.add(i * page_size);
            assert_eq!(
                byte,
                (i as u8).wrapping_add(0x11),
                "page {i}: client backing not populated by RMA write",
            );
        }
    }
}

#[test]
fn rpc_passes_bulk_source_to_handler() {
    if skip_ffi() {
        return;
    }
    let (server, client, server_mr, client_mr, _n_pages, page_size) = paired_fabrics(1);

    let seen = Arc::new(Mutex::new(None));
    let handler = Arc::new(RecordingHandler { seen: seen.clone() });
    let _server_handle = Fabric::start_rpc_server::<TestReq, _>(
        &server,
        handler,
        Some(server_mr),
        crate::memory::HUGEPAGE_2MB,
    )
    .expect("start_rpc_server failed after provider availability gate");

    let transport = FabricTransport::<TestReq, _>::new(
        client.clone(),
        client_mr,
        StaticPeer { peer: PeerId(2) },
        page_size,
    )
    .expect("FabricTransport::new failed after provider availability gate");

    let req = TestReq { tag: 0xACE0 };
    let dsts = vec![PageRef {
        page_idx: 0,
        offset: 0,
        len: page_size as u32,
    }];
    let src = BulkRef {
        stripe: StripeKey([0xA5; 32]),
        offset: 0x0102_0304_0506_0708,
        len: 0x1122_3344,
    };
    let mut stream = transport.bulk_get(&req, src, &dsts);
    // SAFETY: stream is pinned on the stack and never moved.
    let stream = unsafe { std::pin::Pin::new_unchecked(&mut stream) };
    let _results = drain_stream(stream, Duration::from_secs(10));

    assert_eq!(
        *seen.lock().expect("recording handler lock poisoned"),
        Some(src),
        "handler did not receive the requested BulkRef"
    );
}

#[test]
fn rpc_server_error_propagates() {
    if skip_ffi() {
        return;
    }
    let (server, client, server_mr, client_mr, _n_pages, page_size) = paired_fabrics(1);

    let handler = Arc::new(ErrorHandler::<TestErr>::default());
    let _server_handle = Fabric::start_rpc_server::<TestReq, _>(
        &server,
        handler,
        Some(server_mr),
        crate::memory::HUGEPAGE_2MB,
    )
    .expect("start_rpc_server failed after provider availability gate");

    let transport = FabricTransport::<TestReq, _>::new(
        client.clone(),
        client_mr,
        StaticPeer { peer: PeerId(2) },
        page_size,
    )
    .expect("FabricTransport::new failed after provider availability gate");

    let req = TestReq { tag: 0xBEEF };
    let dsts = vec![PageRef {
        page_idx: 0,
        offset: 0,
        len: page_size as u32,
    }];
    let src = BulkRef {
        stripe: StripeKey([0u8; 32]),
        offset: 0,
        len: page_size as u32,
    };
    let mut stream = transport.bulk_get(&req, src, &dsts);
    // SAFETY: stream pinned to this stack frame.
    let stream = unsafe { std::pin::Pin::new_unchecked(&mut stream) };
    let results = drain_stream(stream, Duration::from_secs(10));

    // Expect at least one Err in the stream.
    let any_err = results.iter().any(|r| r.is_err());
    assert!(
        any_err,
        "rpc_server_error_propagates: stream produced {} results, none Err",
        results.len(),
    );
}

#[test]
fn rpc_mid_stream_cancellation() {
    if skip_ffi() {
        return;
    }
    let (server, client, server_mr, client_mr, _n_pages, page_size) = paired_fabrics(2);

    let dropped = Arc::new(AtomicBool::new(false));
    let first_page = PageRef {
        page_idx: 0,
        offset: 0,
        len: page_size as u32,
    };
    let handler = Arc::new(CancelObservingHandler {
        dropped: dropped.clone(),
        page: Some(first_page),
        instances: std::sync::atomic::AtomicUsize::new(0),
    });
    let server_handle = Fabric::start_rpc_server::<TestReq, _>(
        &server,
        handler,
        Some(server_mr),
        crate::memory::HUGEPAGE_2MB,
    )
    .expect("start_rpc_server failed after provider availability gate");

    let transport = FabricTransport::<TestReq, _>::new(
        client.clone(),
        client_mr,
        StaticPeer { peer: PeerId(2) },
        page_size,
    )
    .expect("FabricTransport::new failed after provider availability gate");

    let req = TestReq { tag: 0x1234 };
    let dsts = vec![
        PageRef {
            page_idx: 0,
            offset: 0,
            len: page_size as u32,
        },
        PageRef {
            page_idx: 1,
            offset: 0,
            len: page_size as u32,
        },
    ];
    let src = BulkRef {
        stripe: StripeKey([0u8; 32]),
        offset: 0,
        len: page_size as u32,
    };
    let mut stream = transport.bulk_get(&req, src, &dsts);
    {
        // SAFETY: stream pinned within this block; we drop it
        // explicitly to trigger the client-side cancellation path.
        let mut s = unsafe { std::pin::Pin::new_unchecked(&mut stream) };
        // Poll until we observe the first PAGE_ACK or hit a timeout.
        use std::task::{Context, Poll};
        let waker = crate::runtime::noop_waker();
        let mut cx = Context::from_waker(&waker);
        let started = std::time::Instant::now();
        let mut got_first = false;
        while started.elapsed() < Duration::from_secs(10) && !got_first {
            match s.as_mut().poll_next(&mut cx) {
                Poll::Ready(Some(Ok(_))) => {
                    got_first = true;
                }
                Poll::Ready(Some(Err(e))) => {
                    panic!("unexpected stream error: {e}");
                }
                Poll::Ready(None) => {
                    panic!("stream ended early");
                }
                Poll::Pending => std::thread::sleep(Duration::from_millis(5)),
            }
        }
        assert!(got_first, "did not receive first page within timeout");
        // Drop the stream (and the temporary Pin), triggering
        // client-side fi_cancels on outstanding recvs.
    }
    drop(stream);

    // Now drop the server handle to set the shutdown flag, so the
    // server worker observes it and drops the handler stream. The
    // dropped flag should fire within a bounded interval.
    drop(server_handle);

    let waited = std::time::Instant::now();
    while !dropped.load(Ordering::Acquire) && waited.elapsed() < Duration::from_secs(5) {
        std::thread::sleep(Duration::from_millis(10));
    }
    assert!(
        dropped.load(Ordering::Acquire),
        "server-side handler stream was not dropped within timeout",
    );
}

// ---------------------------------------------------------------
// Production handler roundtrip: a peer fetches a stripe the server
// has resident locally, served by `fabric::PoolHandler` over a
// dedicated scratch backing. Mirrors the canned-handler tests above
// but exercises the real production handler end to end.
// ---------------------------------------------------------------

use crate::bufferpool::{BlockStore, Error as PoolError};
use crate::memory::Backing;

/// In-memory `BlockStore` used as the server's local store. On a
/// hit it writes a known fill byte into the destination scratch page
/// (`PageRef.page_idx`) within `base`; on a miss it returns
/// `Ok(false)`. Mirrors what `LiveShardLocalStore::read_page` does
/// for the production handler without needing a real disk engine.
struct MemBlockStore {
    base: usize,
    page_size: usize,
    present: std::collections::HashMap<[u8; 32], u8>,
}

// SAFETY: `base` points into a leaked, pinned backing that outlives
// the test process; the production `Handler` bound requires
// `Send + Sync` and the bytes are only ever written from the RPC
// worker thread.
unsafe impl Send for MemBlockStore {}
unsafe impl Sync for MemBlockStore {}

impl BlockStore for MemBlockStore {
    fn register_pages(&self, _backing: &Backing) -> Result<(), PoolError> {
        Ok(())
    }

    async fn read_page(
        &self,
        key: StripeKey,
        _stripe_off: u64,
        dst: PageRef,
    ) -> Result<bool, PoolError> {
        match self.present.get(&key.0) {
            Some(&fill) => {
                // SAFETY: dst.page_idx indexes into the scratch
                // backing the test allocated and registered.
                unsafe {
                    let p = (self.base as *mut u8).add(dst.page_idx as usize * self.page_size);
                    std::ptr::write_bytes(p, fill, self.page_size);
                }
                Ok(true)
            }
            None => Ok(false),
        }
    }

    async fn write_page(
        &self,
        _key: StripeKey,
        _stripe_off: u64,
        _page: PageRef,
    ) -> Result<(), PoolError> {
        Ok(())
    }
}

/// Build a server fabric whose RPC server runs the production
/// `PoolHandler` over a scratch backing, plus a client fabric and
/// transport. Returns (server, client, client_mr, transport,
/// page_size, server_handle). The scratch backing for the server is
/// owned by the `MemBlockStore` fill closure via its leaked base.
fn paired_with_pool_handler(
    present: &[([u8; 32], u8)],
) -> (
    Arc<Fabric>,
    crate::fabric::FabricTransport<TestReq, StaticPeer>,
    usize,
    crate::fabric::RpcServerHandle,
    MrHandle,
    Arc<Fabric>,
) {
    let server = Arc::new(new_tcp_fabric_with_peer(PeerId(2)));
    let client = Arc::new(new_tcp_fabric_with_peer(PeerId(1)));

    let page_size = crate::memory::HUGEPAGE_2MB;
    let scratch_pages = 4usize;
    let server_scratch = crate::memory::allocate(crate::memory::BackingRequest {
        kind: crate::memory::BackingKind::Heap,
        bytes: page_size * scratch_pages,
        numa: None,
    })
    .expect("server scratch backing");
    let client_backing = crate::memory::allocate(crate::memory::BackingRequest {
        kind: crate::memory::BackingKind::Heap,
        bytes: page_size,
        numa: None,
    })
    .expect("client backing");
    // Zero the client side so we can detect bytes that came in.
    // SAFETY: client_backing covers `page_size`.
    unsafe {
        std::ptr::write_bytes(client_backing.base, 0, page_size);
    }

    let server_mr = server
        .register_backing(&server_scratch, None)
        .expect("server scratch MR");
    let client_mr = client
        .register_backing(&client_backing, None)
        .expect("client MR");

    let server_addr = wire_addr_of(&server);
    let client_addr = wire_addr_of(&client);
    server
        .add_connection(ConnectionSpec {
            peer: PeerId(1),
            wire_addr: client_addr,
            hca_numa: None,
            labels: Vec::new(),
        })
        .expect("server.add_connection");
    client
        .add_connection(ConnectionSpec {
            peer: PeerId(2),
            wire_addr: server_addr,
            hca_numa: None,
            labels: Vec::new(),
        })
        .expect("client.add_connection");

    let store = Arc::new(MemBlockStore {
        base: server_scratch.base as usize,
        page_size,
        present: present.iter().copied().collect(),
    });
    let handler = Arc::new(PoolHandler::new(
        store,
        server_scratch,
        scratch_pages as u32,
    ));
    let server_handle =
        Fabric::start_rpc_server::<TestReq, _>(&server, handler, Some(server_mr), page_size)
            .expect("start_rpc_server");

    let transport = FabricTransport::<TestReq, _>::new(
        client.clone(),
        client_mr,
        StaticPeer { peer: PeerId(2) },
        page_size,
    )
    .expect("FabricTransport::new");

    // Leak the client backing so it outlives the transport.
    Box::leak(Box::new(client_backing));

    (
        server,
        transport,
        page_size,
        server_handle,
        client_mr,
        client,
    )
}

#[test]
fn pool_handler_serves_resident_stripe() {
    if skip_ffi() {
        return;
    }
    // The store must be keyed under exactly what the request advertises
    // (`TestReq::key()`), since `PoolHandler` reads via `req.key()`.
    let req = TestReq { tag: 1 };
    let key = stripe_for_tag(1);
    let fill = 0xC7u8;
    let (_server, transport, page_size, _server_handle, client_mr, _client) =
        paired_with_pool_handler(&[(key, fill)]);

    let dsts = vec![PageRef {
        page_idx: 0,
        offset: 0,
        len: page_size as u32,
    }];
    let src = BulkRef {
        stripe: StripeKey(key),
        offset: 0,
        len: page_size as u32,
    };
    let mut stream = transport.bulk_get(&req, src, &dsts);
    // SAFETY: stream pinned to this stack frame.
    let stream = unsafe { std::pin::Pin::new_unchecked(&mut stream) };
    let results = drain_stream(stream, Duration::from_secs(20));

    assert_eq!(results.len(), 1, "expected one delivered page");
    assert!(
        results[0].is_ok(),
        "unexpected stream error: {:?}",
        results[0]
    );

    // The client backing page 0 must now hold the server's fill byte,
    // proving the local read landed and was RMA-written to the peer.
    // SAFETY: client_mr.base is the live mapping.
    unsafe {
        let byte = *(client_mr.base as *const u8);
        assert_eq!(byte, fill, "client page not populated from local read");
    }
}

#[test]
fn pool_handler_reports_miss_for_non_resident_stripe() {
    if skip_ffi() {
        return;
    }
    // The store has no entry for `key`, so `read_page` returns a miss
    // and the handler surfaces an error rather than zero bytes.
    let key = [0x11u8; 32];
    let (_server, transport, page_size, _server_handle, _client_mr, _client) =
        paired_with_pool_handler(&[]);

    let req = TestReq { tag: 2 };
    let dsts = vec![PageRef {
        page_idx: 0,
        offset: 0,
        len: page_size as u32,
    }];
    let src = BulkRef {
        stripe: StripeKey(key),
        offset: 0,
        len: page_size as u32,
    };
    let mut stream = transport.bulk_get(&req, src, &dsts);
    // SAFETY: stream pinned to this stack frame.
    let stream = unsafe { std::pin::Pin::new_unchecked(&mut stream) };
    let results = drain_stream(stream, Duration::from_secs(10));

    let any_err = results.iter().any(|r| r.is_err());
    assert!(
        any_err,
        "non-resident stripe must surface an error, got {} ok results",
        results.len(),
    );
}

// ---------------------------------------------------------------
// Phase 4: live multi-hop recursive routing.
//
// These tests stand up a real three-node chain over tcp loopback and
// drive the production `RecursiveHandler` forward path end to end:
//
//   A (client) --static--> B (relay) --finger--> C (owner)
//
// A is a plain client whose `StaticPeer` router always targets B. B
// owns no stripe for the requested key, so its finger table forwards
// to C, RDMA-landing C's served page directly into B's scratch before
// B relays it upstream to A. C owns the key and serves it from its
// local store (or, on a miss, from the origin backend). This is the
// only place the recursive forward path is exercised against a live
// fabric; the per-node routing math is unit-tested in `p2p`.
//
// The finger tables are hand-crafted per node (B knows only C, C knows
// only B) so the chain forwards exactly once at B and terminates at C.
// A globally-consistent three-node ring would let B route straight to
// C in one hop; the point here is to drive a genuine relay hop.
//
// Every test runtime-skips when the libfabric tcp provider is absent.
// ---------------------------------------------------------------

use std::collections::HashMap;

use crate::backend::{Backend, NullBackend};
use crate::p2p::{
    FingerTable, FingerTableConfig, NodeId, PeerEntry, RecursiveHandler, RingId, TopologyLabels,
};

/// Ring position the relay (B) forwards for and the owner (C) serves.
/// Chosen to sit in the open arc `(B.ring, C.ring)` so B's finger
/// lookup forwards to C and C owns it.
const CHAIN_TARGET_RING: u64 = 150;

/// Request whose stripe key's leading 8 bytes encode a ring id, so
/// `stripe_to_ring(req.key())` is controllable from the test.
#[derive(Clone, Serialize, Deserialize, Debug)]
struct RingReq {
    key_bytes: [u8; 32],
}

impl Req for RingReq {
    fn key(&self) -> StripeKey {
        StripeKey(self.key_bytes)
    }
}

/// Origin backend that fills its destination scratch page with a known
/// byte then yields it once, standing in for an authoritative origin
/// fetch on an owner-side store miss.
struct FillBackend {
    base: usize,
    page_size: usize,
    fill: u8,
}

// SAFETY: `base` points into a leaked scratch backing that outlives
// the test; the page is only written from the RPC worker thread.
unsafe impl Send for FillBackend {}
unsafe impl Sync for FillBackend {}

struct FillStream {
    page: Option<PageRef>,
}

impl PageStream for FillStream {
    fn poll_next(
        mut self: Pin<&mut Self>,
        _cx: &mut Context<'_>,
    ) -> Poll<Option<Result<PageRef, PoolError>>> {
        match self.page.take() {
            Some(p) => Poll::Ready(Some(Ok(p))),
            None => Poll::Ready(None),
        }
    }
}

impl Backend for FillBackend {
    type Req = RingReq;
    type Stream<'a> = FillStream;

    fn bulk_get<'a>(
        &'a self,
        _req: &'a Self::Req,
        _src: BulkRef,
        dsts: &'a [PageRef],
    ) -> Self::Stream<'a> {
        let dst = dsts[0];
        // SAFETY: dst.page_idx indexes the scratch backing at `base`.
        unsafe {
            let p = (self.base as *mut u8).add(dst.page_idx as usize * self.page_size);
            std::ptr::write_bytes(p, self.fill, self.page_size);
        }
        FillStream { page: Some(dst) }
    }
}

/// Which origin backend the owner (C) consults on a store miss.
enum OwnerBackend {
    /// Inert backend: a store miss surfaces an error up the chain.
    Null,
    /// Origin fetch that fills the page with the given byte.
    Fill(u8),
}

/// Build a `PeerEntry` at an explicit ring position. The ring is set
/// directly (not via `node_to_ring`) so the test controls the layout.
fn ring_peer(node: u64, ring: u64) -> PeerEntry {
    PeerEntry {
        node: NodeId(node),
        ring: RingId(ring),
        labels: TopologyLabels(vec!["r".to_string()]),
    }
}

/// A `StripeKey` whose leading 8 bytes little-endian equal `ring`.
fn key_for_ring(ring: u64) -> StripeKey {
    let mut k = [0u8; 32];
    k[..8].copy_from_slice(&ring.to_le_bytes());
    StripeKey(k)
}

/// Number of scratch pages each relay/owner node registers. Deliberately
/// greater than one: `ScratchBacking::take()` pops the highest free index
/// first, so a forwarding node reserves a non-zero scratch slot. This
/// exercises the forward path's per-slot RMA destination offset and guards
/// against a regression where the downstream hop writes into ordinal 0
/// while the relay reads back from `PageRef.page_idx`.
const RECURSIVE_SCRATCH_PAGES: usize = 8;

/// Start a node running the production `RecursiveHandler` over a
/// `RECURSIVE_SCRATCH_PAGES`-page scratch backing.
#[allow(clippy::too_many_arguments)]
fn start_recursive_node<B>(
    fabric: &Arc<Fabric>,
    scratch: Backing,
    scratch_mr: MrHandle,
    page_size: usize,
    fingers: Arc<FingerTable>,
    node_to_peer: Arc<HashMap<NodeId, PeerId>>,
    store: Arc<MemBlockStore>,
    backend: B,
) -> crate::fabric::RpcServerHandle
where
    B: Backend<Req = RingReq> + 'static,
{
    let handler = Arc::new(
        RecursiveHandler::new(
            store,
            scratch,
            RECURSIVE_SCRATCH_PAGES as u32,
            fingers,
            node_to_peer,
            fabric.clone(),
            scratch_mr,
            page_size,
            backend,
        )
        .expect("RecursiveHandler::new failed after provider availability gate"),
    );
    Fabric::start_rpc_server::<RingReq, _>(fabric, handler, Some(scratch_mr), page_size)
        .expect("start_rpc_server failed after provider availability gate")
}

/// Stand up the live A -> B -> C chain. `c_present` lists `(ring, fill)`
/// entries resident in C's store; `owner` selects C's origin backend
/// for the store-miss case. Returns the client transport, the client
/// MR, the page size, both server handles, and the three fabrics. The
/// handles and fabrics must be held for the lifetime of the test.
#[allow(clippy::type_complexity)]
fn recursive_chain(
    c_present: &[(u64, u8)],
    owner: OwnerBackend,
) -> (
    FabricTransport<RingReq, StaticPeer>,
    MrHandle,
    usize,
    crate::fabric::RpcServerHandle,
    crate::fabric::RpcServerHandle,
    Arc<Fabric>,
    Arc<Fabric>,
    Arc<Fabric>,
) {
    let a = Arc::new(new_tcp_fabric_with_peer(PeerId(1)));
    let b = Arc::new(new_tcp_fabric_with_peer(PeerId(2)));
    let c = Arc::new(new_tcp_fabric_with_peer(PeerId(3)));
    let page_size = crate::memory::HUGEPAGE_2MB;

    let alloc = |bytes| {
        crate::memory::allocate(crate::memory::BackingRequest {
            kind: crate::memory::BackingKind::Heap,
            bytes,
            numa: None,
        })
        .expect("heap backing allocation failed")
    };
    let b_scratch = alloc(page_size * RECURSIVE_SCRATCH_PAGES);
    let c_scratch = alloc(page_size * RECURSIVE_SCRATCH_PAGES);
    let client_backing = alloc(page_size);
    // Zero the client page so a populated byte proves an RMA landing.
    // SAFETY: client_backing covers `page_size`.
    unsafe {
        std::ptr::write_bytes(client_backing.base, 0, page_size);
    }

    let b_base = b_scratch.base as usize;
    let c_base = c_scratch.base as usize;

    let b_mr = b.register_backing(&b_scratch, None).expect("b scratch MR");
    let c_mr = c.register_backing(&c_scratch, None).expect("c scratch MR");
    let client_mr = a
        .register_backing(&client_backing, None)
        .expect("client MR");

    // Global peer-id scheme: A=1, B=2, C=3 in every fabric's table.
    let a_addr = wire_addr_of(&a);
    let b_addr = wire_addr_of(&b);
    let c_addr = wire_addr_of(&c);
    a.add_connection(ConnectionSpec {
        peer: PeerId(2),
        wire_addr: b_addr.clone(),
        hca_numa: None,
        labels: Vec::new(),
    })
    .expect("a -> b connection");
    b.add_connection(ConnectionSpec {
        peer: PeerId(1),
        wire_addr: a_addr,
        hca_numa: None,
        labels: Vec::new(),
    })
    .expect("b -> a connection");
    b.add_connection(ConnectionSpec {
        peer: PeerId(3),
        wire_addr: c_addr,
        hca_numa: None,
        labels: Vec::new(),
    })
    .expect("b -> c connection");
    c.add_connection(ConnectionSpec {
        peer: PeerId(2),
        wire_addr: b_addr,
        hca_numa: None,
        labels: Vec::new(),
    })
    .expect("c -> b connection");

    // B (ring 100) forwards CHAIN_TARGET_RING (150) to its successor C
    // (ring 200); C owns it. Each table knows only the other node.
    let b_fingers = Arc::new(FingerTable::build(
        ring_peer(2, 100),
        std::slice::from_ref(&ring_peer(3, 200)),
        FingerTableConfig { k: 8 },
    ));
    let c_fingers = Arc::new(FingerTable::build(
        ring_peer(3, 200),
        std::slice::from_ref(&ring_peer(2, 100)),
        FingerTableConfig { k: 8 },
    ));
    let b_n2p: Arc<HashMap<NodeId, PeerId>> =
        Arc::new([(NodeId(3), PeerId(3))].into_iter().collect());
    // C never forwards for the target; mapping is present for symmetry.
    let c_n2p: Arc<HashMap<NodeId, PeerId>> =
        Arc::new([(NodeId(2), PeerId(2))].into_iter().collect());

    let b_store = Arc::new(MemBlockStore {
        base: b_base,
        page_size,
        present: HashMap::new(),
    });
    let c_store = Arc::new(MemBlockStore {
        base: c_base,
        page_size,
        present: c_present
            .iter()
            .map(|&(ring, fill)| (key_for_ring(ring).0, fill))
            .collect(),
    });

    // B always forwards: empty store, inert backend.
    let b_handle = start_recursive_node(
        &b,
        b_scratch,
        b_mr,
        page_size,
        b_fingers,
        b_n2p,
        b_store,
        NullBackend::<RingReq>::new(),
    );
    // C owns the key; its backend depends on the scenario.
    let c_handle = match owner {
        OwnerBackend::Null => start_recursive_node(
            &c,
            c_scratch,
            c_mr,
            page_size,
            c_fingers,
            c_n2p,
            c_store,
            NullBackend::<RingReq>::new(),
        ),
        OwnerBackend::Fill(fill) => start_recursive_node(
            &c,
            c_scratch,
            c_mr,
            page_size,
            c_fingers,
            c_n2p,
            c_store,
            FillBackend {
                base: c_base,
                page_size,
                fill,
            },
        ),
    };

    let transport = FabricTransport::<RingReq, _>::new(
        a.clone(),
        client_mr,
        StaticPeer { peer: PeerId(2) },
        page_size,
    )
    .expect("client FabricTransport::new failed after provider availability gate");

    // Leak the client backing so it outlives the transport.
    Box::leak(Box::new(client_backing));

    (transport, client_mr, page_size, b_handle, c_handle, a, b, c)
}

/// Issue the chain request for `CHAIN_TARGET_RING` against one client
/// page and collect the stream results.
fn run_chain_request(
    transport: &FabricTransport<RingReq, StaticPeer>,
    page_size: usize,
    timeout: Duration,
) -> Vec<Result<PageRef, PoolError>> {
    let key = key_for_ring(CHAIN_TARGET_RING);
    let req = RingReq { key_bytes: key.0 };
    let dsts = vec![PageRef {
        page_idx: 0,
        offset: 0,
        len: page_size as u32,
    }];
    let src = BulkRef {
        stripe: key,
        offset: 0,
        len: page_size as u32,
    };
    let mut stream = transport.bulk_get(&req, src, &dsts);
    // SAFETY: stream pinned to this stack frame and never moved.
    let stream = unsafe { Pin::new_unchecked(&mut stream) };
    drain_stream(stream, timeout)
}

#[test]
fn recursive_two_hop_owner_serves_from_store() {
    if skip_ffi() {
        return;
    }
    let fill = 0xC7u8;
    let (transport, client_mr, page_size, _bh, _ch, _a, _b, _c) =
        recursive_chain(&[(CHAIN_TARGET_RING, fill)], OwnerBackend::Null);

    let results = run_chain_request(&transport, page_size, Duration::from_secs(30));

    assert_eq!(results.len(), 1, "expected one relayed page");
    assert!(
        results[0].is_ok(),
        "unexpected stream error: {:?}",
        results[0]
    );

    // The client page must hold C's store fill byte, proving the page
    // traversed C -> B scratch -> A's MR over the live forward path.
    // SAFETY: client_mr.base is the live client mapping.
    unsafe {
        let byte = *(client_mr.base as *const u8);
        assert_eq!(byte, fill, "client page not populated via two-hop relay");
    }
}

#[test]
fn recursive_owner_miss_falls_through_to_backend() {
    if skip_ffi() {
        return;
    }
    let fill = 0x3Bu8;
    // C's store is empty for the key, so it falls through to the origin
    // backend, which fills the page.
    let (transport, client_mr, page_size, _bh, _ch, _a, _b, _c) =
        recursive_chain(&[], OwnerBackend::Fill(fill));

    let results = run_chain_request(&transport, page_size, Duration::from_secs(30));

    assert_eq!(results.len(), 1, "expected one relayed page");
    assert!(
        results[0].is_ok(),
        "unexpected stream error: {:?}",
        results[0]
    );

    // SAFETY: client_mr.base is the live client mapping.
    unsafe {
        let byte = *(client_mr.base as *const u8);
        assert_eq!(byte, fill, "client page not populated from origin backend");
    }
}

#[test]
fn recursive_owner_miss_null_backend_errors() {
    if skip_ffi() {
        return;
    }
    // C's store is empty and its origin backend is inert, so the miss
    // surfaces as an error that propagates back through B to A.
    let (transport, _client_mr, page_size, _bh, _ch, _a, _b, _c) =
        recursive_chain(&[], OwnerBackend::Null);

    let results = run_chain_request(&transport, page_size, Duration::from_secs(30));

    let err_msg = results
        .iter()
        .find_map(|r| r.as_ref().err().map(|e| e.to_string()));
    assert!(
        err_msg
            .as_deref()
            .is_some_and(|m| m.contains("no backend configured")),
        "expected owner-miss error to propagate up the chain, got {err_msg:?}",
    );
}

#[test]
fn zero_ttl_owner_serves_locally() {
    if skip_ffi() {
        return;
    }
    // A request that arrives with a zero hop budget at the stripe OWNER
    // must still be served locally: the hop budget only bounds
    // forwarding, not owner-serves. The server runs a `PoolHandler`
    // (which never forwards and always owns), so a `ttl == 0` request is
    // handled rather than rejected. With an empty store the local serve
    // misses and surfaces as a "not resident" error, NOT a "hop limit
    // exceeded" rejection. This is the integration-level guard for the
    // M3 fix (the old unconditional TTL guard wrongly refused this).
    let (_server, transport, page_size, _server_handle, _client_mr, _client) =
        paired_with_pool_handler(&[]);

    let req = TestReq { tag: 7 };
    let dsts = vec![PageRef {
        page_idx: 0,
        offset: 0,
        len: page_size as u32,
    }];
    let src = BulkRef {
        stripe: StripeKey([0u8; 32]),
        offset: 0,
        len: page_size as u32,
    };
    let mut stream = transport.bulk_get_with_ttl(&req, src, &dsts, 0);
    // SAFETY: stream pinned to this stack frame and never moved.
    let stream = unsafe { Pin::new_unchecked(&mut stream) };
    let results = drain_stream(stream, Duration::from_secs(10));

    let err_msg = results
        .iter()
        .find_map(|r| r.as_ref().err().map(|e| e.to_string()));
    assert!(
        err_msg
            .as_deref()
            .is_some_and(|m| m.contains("not resident")),
        "expected owner-serve miss (not a hop-limit rejection), got {err_msg:?}",
    );
}
