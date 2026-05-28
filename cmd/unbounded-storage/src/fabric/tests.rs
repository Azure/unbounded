// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Module integration tests for the fabric module. Covers connection
//! CRUD, MR registration, and ping/pong round-trip over a loopback
//! tcp fabric. Each test skips during setup if explicitly requested or
//! if the tcp provider is not installed in the libfabric build
//! available to the test binary.

use std::ffi::CString;
use std::ptr;
use std::sync::Arc;
use std::time::Duration;

use crate::bufferpool::PeerId;
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
            ffi::FI_VERSION,
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
    cfg
}

fn new_tcp_fabric() -> Fabric {
    Fabric::new(tcp_loopback_cfg())
        .expect("Fabric::new tcp failed after provider availability gate")
}

fn hex_encode(bytes: &[u8]) -> String {
    const HEX: &[u8; 16] = b"0123456789abcdef";
    let mut s = String::with_capacity(bytes.len() * 2);
    for b in bytes {
        s.push(HEX[(b >> 4) as usize] as char);
        s.push(HEX[(b & 0x0f) as usize] as char);
    }
    s
}

/// Derive a `wire_addr` for `peer_fabric` understood by this build's
/// provider. For tcp the self-address is a sockaddr blob; we
/// stringify it as "ip:port" using inet_ntop / ntohs to match the
/// shim's getaddrinfo parser.
fn wire_addr_of(peer_fabric: &Fabric) -> String {
    let raw = peer_fabric.self_address().expect("self_address");
    let addr = decode_sockaddr_to_string(&raw);
    assert!(
        !addr.is_empty(),
        "could not stringify tcp self-address: {}",
        hex_encode(&raw),
    );
    addr
}

fn decode_sockaddr_to_string(raw: &[u8]) -> String {
    // raw begins with sa_family (u16 little-endian on Linux).
    if raw.len() < 2 {
        return String::new();
    }
    let family = u16::from_ne_bytes([raw[0], raw[1]]);
    const AF_INET: u16 = libc::AF_INET as u16;
    const AF_INET6: u16 = libc::AF_INET6 as u16;
    if family == AF_INET && raw.len() >= 8 {
        // sockaddr_in: family (2) + port (2 BE) + addr (4) + ...
        let port = u16::from_be_bytes([raw[2], raw[3]]);
        let ip = format!("{}.{}.{}.{}", raw[4], raw[5], raw[6], raw[7]);
        format!("{ip}:{port}")
    } else if family == AF_INET6 && raw.len() >= 28 {
        // sockaddr_in6: family (2) + port (2 BE) + flowinfo (4) +
        // addr (16) + scope (4).
        let port = u16::from_be_bytes([raw[2], raw[3]]);
        let mut groups = [0u16; 8];
        for i in 0..8 {
            groups[i] = u16::from_be_bytes([raw[8 + 2 * i], raw[8 + 2 * i + 1]]);
        }
        let ip = format!(
            "{:x}:{:x}:{:x}:{:x}:{:x}:{:x}:{:x}:{:x}",
            groups[0], groups[1], groups[2], groups[3], groups[4], groups[5], groups[6], groups[7],
        );
        format!("[{ip}]:{port}")
    } else {
        String::new()
    }
}

#[test]
fn tcp_loopback_ping_roundtrip() {
    if skip_ffi() {
        return;
    }
    let a = new_tcp_fabric();
    let b = new_tcp_fabric();

    let a_addr = wire_addr_of(&a);
    let b_addr = wire_addr_of(&b);

    let a_to_b = ConnectionSpec {
        peer: PeerId(2),
        wire_addr: b_addr,
        hca_numa: None,
    };
    let b_to_a = ConnectionSpec {
        peer: PeerId(1),
        wire_addr: a_addr,
        hca_numa: None,
    };
    a.add_connection(a_to_b)
        .expect("add_connection a->b failed after provider availability gate");
    b.add_connection(b_to_a)
        .expect("add_connection b->a failed after provider availability gate");

    match a.ping(PeerId(2), Duration::from_secs(2)) {
        Ok(latency) => {
            assert!(
                latency < Duration::from_secs(1),
                "ping latency too high: {latency:?}"
            );
        }
        Err(e) => {
            panic!("ping returned {e}");
        }
    }
    drop(a);
    drop(b);
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
    };
    f.add_connection(spec.clone()).expect("add 1");
    assert!(f.list_connections().contains(&PeerId(42)));
    f.remove_connection(PeerId(42)).expect("remove");
    assert!(!f.list_connections().contains(&PeerId(42)));
    f.add_connection(spec).expect("add 2");
    assert!(f.list_connections().contains(&PeerId(42)));
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
    let backing = crate::backing::allocate(crate::backing::BackingRequest {
        kind: crate::backing::BackingKind::Heap,
        bytes: crate::backing::HUGEPAGE_2MB,
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

#[test]
fn ping_unknown_peer_errors() {
    if skip_ffi() {
        return;
    }
    let f = new_tcp_fabric();
    match f.ping(PeerId(999), Duration::from_millis(50)) {
        Err(FabricError::NotFound(_)) => {}
        other => panic!("expected NotFound, got {other:?}"),
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
use crate::fabric::{FabricTransport, Handler, HandlerStream, MrHandle, StaticPeer};

#[derive(Clone, Serialize, Deserialize, Debug)]
struct TestReq {
    nonce: u64,
}

impl Req for TestReq {
    fn key(&self) -> StripeKey {
        StripeKey([0u8; 32])
    }
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

    fn handle<'a>(&'a self, _req: &'a R, src: BulkRef) -> Self::Stream<'a> {
        *self.seen.lock().expect("recording handler lock poisoned") = Some(src);
        EmptyStream
    }
}

/// Bring up a paired (server, client) pair of fabrics over tcp
/// loopback. Returns (server, client, server_local_mr,
/// client_local_mr, n_pages, page_size).
fn paired_fabrics(n_pages: usize) -> (Arc<Fabric>, Arc<Fabric>, MrHandle, MrHandle, usize, usize) {
    let server = Arc::new(new_tcp_fabric());
    let client = Arc::new(new_tcp_fabric());

    let page_size = crate::backing::HUGEPAGE_2MB;
    let bytes = page_size * n_pages;
    let server_backing = crate::backing::allocate(crate::backing::BackingRequest {
        kind: crate::backing::BackingKind::Heap,
        bytes,
        numa: None,
    })
    .expect("server heap backing allocation failed");
    let client_backing = crate::backing::allocate(crate::backing::BackingRequest {
        kind: crate::backing::BackingKind::Heap,
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
        })
        .expect("server.add_connection failed after provider availability gate");
    client
        .add_connection(ConnectionSpec {
            peer: PeerId(2), // server peer-id in client's table
            wire_addr: server_addr,
            hca_numa: None,
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
    use std::ptr;
    use std::task::{Context, Poll, RawWaker, RawWakerVTable, Waker};

    fn no(_: *const ()) {}
    fn clone(_: *const ()) -> RawWaker {
        RawWaker::new(ptr::null(), &VT)
    }
    static VT: RawWakerVTable = RawWakerVTable::new(clone, no, no, no);
    // SAFETY: vtable never deref's the data pointer.
    let waker = unsafe { Waker::from_raw(RawWaker::new(ptr::null(), &VT)) };
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
    let _server_handle = Fabric::start_rpc_server::<TestReq, _>(&server, handler, Some(server_mr))
        .expect("start_rpc_server failed after provider availability gate");

    let transport = FabricTransport::<TestReq, _>::new(
        client.clone(),
        client_mr,
        StaticPeer { peer: PeerId(2) },
        page_size,
    )
    .expect("FabricTransport::new failed after provider availability gate");

    let req = TestReq { nonce: 0xCAFE };
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
    let _server_handle = Fabric::start_rpc_server::<TestReq, _>(&server, handler, Some(server_mr))
        .expect("start_rpc_server failed after provider availability gate");

    let transport = FabricTransport::<TestReq, _>::new(
        client.clone(),
        client_mr,
        StaticPeer { peer: PeerId(2) },
        page_size,
    )
    .expect("FabricTransport::new failed after provider availability gate");

    let req = TestReq { nonce: 0xACE0 };
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
    let _server_handle = Fabric::start_rpc_server::<TestReq, _>(&server, handler, Some(server_mr))
        .expect("start_rpc_server failed after provider availability gate");

    let transport = FabricTransport::<TestReq, _>::new(
        client.clone(),
        client_mr,
        StaticPeer { peer: PeerId(2) },
        page_size,
    )
    .expect("FabricTransport::new failed after provider availability gate");

    let req = TestReq { nonce: 0xBEEF };
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
    let server_handle = Fabric::start_rpc_server::<TestReq, _>(&server, handler, Some(server_mr))
        .expect("start_rpc_server failed after provider availability gate");

    let transport = FabricTransport::<TestReq, _>::new(
        client.clone(),
        client_mr,
        StaticPeer { peer: PeerId(2) },
        page_size,
    )
    .expect("FabricTransport::new failed after provider availability gate");

    let req = TestReq { nonce: 0x1234 };
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
        use std::ptr;
        use std::task::{Context, Poll, RawWaker, RawWakerVTable, Waker};
        fn no(_: *const ()) {}
        fn clone(_: *const ()) -> RawWaker {
            RawWaker::new(ptr::null(), &VT)
        }
        static VT: RawWakerVTable = RawWakerVTable::new(clone, no, no, no);
        // SAFETY: vtable never derefs the data pointer.
        let waker = unsafe { Waker::from_raw(RawWaker::new(ptr::null(), &VT)) };
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
