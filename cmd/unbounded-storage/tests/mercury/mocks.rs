// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! DST-aware mocks for Mercury's `Transport` (client) and `BulkSource`
//! (server) traits. Each async path routes its "RPC latency" through
//! [`yield_n`] with a per-call random count drawn from the framework's
//! [`SimState::rng`], and may inject synthetic faults governed by the
//! per-area [`MercurySimCfg`]. Counters are exposed so workloads can
//! assert higher-level properties (capacity backpressure, completion
//! totals, ...).
//!
//! The mocks are deliberately `!Send` (they hold `Rc`/`RefCell`); a
//! production `MercuryTransport` is `Send + Sync`, but the DST harness
//! runs everything on a single executor and does not need that. The
//! one place this matters is `BulkSource::fetch`, whose signature
//! returns a `Send` future. We satisfy that bound by computing all
//! random draws and data clones synchronously up front and returning a
//! future whose captured state is only `Send`-safe primitives plus a
//! `Vec<u8>`.

#![allow(dead_code)]

use std::cell::RefCell;
use std::collections::HashMap;
use std::future::Future;
use std::pin::Pin;
use std::rc::Rc;

use rand::Rng;
use serde::{Deserialize, Serialize};

use unbounded_storage::bufferpool::{
    BulkRef, Error as BpError, PageRef, Req, StripeKey, Transport,
};
use unbounded_storage::mercury::{BulkSource, HgError, PeerId};

use crate::framework::executor::{with_sim, yield_n, yield_once};

// =====================================================================
// Configuration
// =====================================================================

/// Mercury-specific simulation knobs that ride alongside the
/// framework's [`SimState`]. Held behind an `Rc` so the client mock,
/// server mock, and the workload driver can share a single instance
/// without leaking knowledge into the framework crate.
pub(crate) struct MercurySimCfg {
    /// Min/max yield count for "RPC latency" in [`MockTransport::bulk_get`]
    /// (drawn from `SimState`'s PRNG on each call).
    pub min_latency_yields: u32,
    pub max_latency_yields: u32,
    /// Probability that an RPC fails outright with [`HgError::HgForward`].
    pub forward_fault_rate: f64,
    /// Probability that a fetch returns fewer bytes than requested,
    /// surfaced to the client as [`HgError::ShortRead`].
    pub short_read_rate: f64,
    /// Probability that a peer is reported as unreachable
    /// ([`HgError::HgAddrLookup`]).
    pub peer_disconnect_rate: f64,
    /// Maximum in-flight RPCs the mock will run concurrently before
    /// backpressuring callers via additional yields.
    pub capacity: u32,
    /// Counters shared with the harness for invariant assertions.
    pub counters: Rc<RefCell<MercuryCounters>>,
}

impl MercurySimCfg {
    pub fn new() -> Rc<Self> {
        Rc::new(Self {
            min_latency_yields: 0,
            max_latency_yields: 0,
            forward_fault_rate: 0.0,
            short_read_rate: 0.0,
            peer_disconnect_rate: 0.0,
            capacity: u32::MAX,
            counters: Rc::new(RefCell::new(MercuryCounters::default())),
        })
    }
}

#[derive(Default)]
pub(crate) struct MercuryCounters {
    pub forwards_started: u64,
    pub forwards_completed_ok: u64,
    pub forwards_completed_err: u64,
    pub bytes_pushed: u64,
    pub fetches_invoked: u64,
    pub fetches_short: u64,
    pub capacity_waits: u64,
    pub peak_in_flight: u32,
    pub current_in_flight: u32,
}

// =====================================================================
// Request type
// =====================================================================

/// Test request type. The pool only inspects `req.key()`; Mercury also
/// requires `Serialize + DeserializeOwned + Send + Sync + 'static` for
/// the on-wire trip, so we derive serde here even though the DST never
/// actually serializes.
#[derive(Clone, Debug, Serialize, Deserialize)]
pub(crate) struct MockReq {
    pub key: [u8; 32],
}

impl Req for MockReq {
    fn key(&self) -> StripeKey {
        StripeKey(self.key)
    }
}

// =====================================================================
// Client transport
// =====================================================================

/// Per-peer reference data: the canonical bytes a `MockBulkSource`
/// will hand out when fetched, and that a `MockTransport` will copy
/// into the destination buffer on success. Shared between the client
/// and server mocks so they agree on payload contents.
pub(crate) type PeerBytes = Rc<RefCell<HashMap<PeerId, Vec<u8>>>>;

/// Client-side mock. Each instance routes to a fixed peer (DST routing
/// is static); the workload constructs one transport per peer it wants
/// to speak to. Writes successful fetches into a shared `dst_buffer`
/// owned by the harness.
pub(crate) struct MockTransport {
    cfg: Rc<MercurySimCfg>,
    bytes_by_peer: PeerBytes,
    peer: PeerId,
    page_size: u32,
    /// Destination buffer the harness pre-allocated. The DST harness
    /// does not pin a real `Backing` through Mercury; instead the
    /// destination is a plain `Vec<u8>` and the mock writes into it
    /// directly using `(page_idx, offset)` from `PageRef`.
    dst_buffer: Rc<RefCell<Vec<u8>>>,
}

impl MockTransport {
    pub fn new(
        cfg: Rc<MercurySimCfg>,
        bytes_by_peer: PeerBytes,
        peer: PeerId,
        page_size: u32,
        dst_buffer: Rc<RefCell<Vec<u8>>>,
    ) -> Self {
        Self {
            cfg,
            bytes_by_peer,
            peer,
            page_size,
            dst_buffer,
        }
    }

    fn dec_in_flight(&self) {
        let mut c = self.cfg.counters.borrow_mut();
        c.current_in_flight = c.current_in_flight.saturating_sub(1);
    }
}

impl Transport<MockReq> for MockTransport {
    async fn bulk_get(&self, _req: &MockReq, src: BulkRef, dst: PageRef) -> Result<(), BpError> {
        // Account the start of an RPC and update the peak watermark.
        {
            let mut c = self.cfg.counters.borrow_mut();
            c.forwards_started += 1;
            c.current_in_flight += 1;
            if c.current_in_flight > c.peak_in_flight {
                c.peak_in_flight = c.current_in_flight;
            }
        }

        // Capacity backpressure: while we're over the cap, yield to
        // give other in-flight RPCs a chance to drain. We count the
        // first wait per call so the harness can assert that capacity
        // was actually exercised.
        let capacity = self.cfg.capacity;
        let over_cap = {
            let c = self.cfg.counters.borrow();
            c.current_in_flight > capacity
        };
        if over_cap {
            self.cfg.counters.borrow_mut().capacity_waits += 1;
            loop {
                yield_once().await;
                if self.cfg.counters.borrow().current_in_flight <= capacity {
                    break;
                }
            }
        }

        // Peer reachability check. A simulated lookup failure is the
        // earliest possible failure mode; account it as a completed
        // error.
        let disconnect_rate = clamp01(self.cfg.peer_disconnect_rate);
        let disconnected = disconnect_rate > 0.0 && with_sim(|s| s.rng.gen_bool(disconnect_rate));
        if disconnected {
            self.cfg.counters.borrow_mut().forwards_completed_err += 1;
            self.dec_in_flight();
            return Err(BpError::transport(HgError::HgAddrLookup(0)));
        }

        // Variable RPC latency. Drawn before any data work so PRNG
        // consumption is stable across reorderings of independent
        // tasks.
        let latency = draw_latency(&self.cfg);
        yield_n(latency).await;

        // Forward fault: terminal RPC error after the latency window.
        let fault_rate = clamp01(self.cfg.forward_fault_rate);
        let faulted = fault_rate > 0.0 && with_sim(|s| s.rng.gen_bool(fault_rate));
        if faulted {
            self.cfg.counters.borrow_mut().forwards_completed_err += 1;
            self.dec_in_flight();
            return Err(BpError::transport(HgError::HgForward(-1)));
        }

        // Short-read fault: server returned fewer bytes than asked
        // for. The contract says non-zero status means the client
        // should treat `dst` as undefined, so we deliberately do not
        // touch `dst_buffer` and surface the `ShortRead` to the
        // caller.
        let short_rate = clamp01(self.cfg.short_read_rate);
        let short = short_rate > 0.0 && with_sim(|s| s.rng.gen_bool(short_rate));
        if short {
            let got = if src.len > 1 {
                with_sim(|s| s.rng.gen_range(0..src.len))
            } else {
                0
            };
            {
                let mut c = self.cfg.counters.borrow_mut();
                c.fetches_short += 1;
                c.forwards_completed_err += 1;
            }
            self.dec_in_flight();
            return Err(BpError::transport(HgError::ShortRead {
                expected: src.len,
                got,
            }));
        }

        // Happy path: copy `[off, off+len)` from the peer's reference
        // bytes into the destination slice.
        let off = src.offset as usize;
        let len = src.len as usize;
        let slice: Vec<u8> = {
            let map = self.bytes_by_peer.borrow();
            let Some(peer_bytes) = map.get(&self.peer) else {
                self.cfg.counters.borrow_mut().forwards_completed_err += 1;
                self.dec_in_flight();
                return Err(BpError::transport(HgError::HgAddrLookup(0)));
            };
            if off.saturating_add(len) > peer_bytes.len() {
                self.cfg.counters.borrow_mut().forwards_completed_err += 1;
                self.dec_in_flight();
                return Err(BpError::transport(HgError::ShortRead {
                    expected: src.len,
                    got: 0,
                }));
            }
            peer_bytes[off..off + len].to_vec()
        };

        {
            let page_size = self.page_size as usize;
            let dst_off = dst.page_idx as usize * page_size + dst.offset as usize;
            let mut buf = self.dst_buffer.borrow_mut();
            assert!(
                dst_off + len <= buf.len(),
                "MockTransport: dst out of range (dst_off={dst_off} len={len} buf.len={})",
                buf.len()
            );
            buf[dst_off..dst_off + len].copy_from_slice(&slice);
        }

        {
            let mut c = self.cfg.counters.borrow_mut();
            c.bytes_pushed += len as u64;
            c.forwards_completed_ok += 1;
        }
        self.dec_in_flight();
        Ok(())
    }
}

// =====================================================================
// Server: BulkSource
// =====================================================================

/// Server-side mock. Owns a shared per-peer byte map (so multiple
/// clients agree on payload contents) and serves bytes for its own
/// `self_peer`. Latency and short-read decisions flow through the
/// same [`MercurySimCfg`] as the client.
pub(crate) struct MockBulkSource {
    cfg: Rc<MercurySimCfg>,
    bytes: PeerBytes,
    self_peer: PeerId,
}

impl MockBulkSource {
    pub fn new(cfg: Rc<MercurySimCfg>, bytes: PeerBytes, self_peer: PeerId) -> Self {
        Self {
            cfg,
            bytes,
            self_peer,
        }
    }
}

// SAFETY: `MockBulkSource` holds `Rc`/`RefCell`, so it is genuinely
// `!Send + !Sync`. The DST harness is single-threaded and the executor
// never moves a future across threads, but `BulkSource<R>` requires
// `Send + Sync + 'static` to match the production trait shape. We
// satisfy the bound here; nothing in the test target ever sends a
// `MockBulkSource` across a real thread boundary.
unsafe impl Send for MockBulkSource {}
unsafe impl Sync for MockBulkSource {}

impl BulkSource<MockReq> for MockBulkSource {
    fn fetch<'a>(
        &'a self,
        _req: &'a MockReq,
        offset: u64,
        len: u32,
    ) -> Pin<Box<dyn Future<Output = std::result::Result<Vec<u8>, HgError>> + Send + 'a>> {
        // The DST harness is single-threaded, but `BulkSource::fetch`
        // requires a `Send` future. We satisfy that by doing all the
        // randomness-and-`Rc` work synchronously here and capturing
        // only `Send` primitives (the latency count and the resulting
        // `Vec<u8>`) into the returned future. No `Rc`/`RefCell`
        // borrow ever crosses an `.await` below.
        self.cfg.counters.borrow_mut().fetches_invoked += 1;

        let latency = draw_latency(&self.cfg);
        let short_rate = clamp01(self.cfg.short_read_rate);
        let short = short_rate > 0.0 && with_sim(|s| s.rng.gen_bool(short_rate));

        let result: std::result::Result<Vec<u8>, HgError> = {
            let map = self.bytes.borrow();
            match map.get(&self.self_peer) {
                None => Err(HgError::HgAddrLookup(0)),
                Some(peer_bytes) => {
                    let off = offset as usize;
                    let want = len as usize;
                    if off.saturating_add(want) > peer_bytes.len() {
                        Ok(Vec::new())
                    } else if short {
                        let actual = if len > 0 {
                            with_sim(|s| s.rng.gen_range(0..len))
                        } else {
                            0
                        };
                        self.cfg.counters.borrow_mut().fetches_short += 1;
                        Ok(peer_bytes[off..off + actual as usize].to_vec())
                    } else {
                        Ok(peer_bytes[off..off + want].to_vec())
                    }
                }
            }
        };

        Box::pin(async move {
            yield_n(latency).await;
            result
        })
    }
}

// =====================================================================
// Construction helpers
// =====================================================================

/// Build a matched set of client transports (one per peer) and a
/// single shared server source. Pre-fills the per-peer reference
/// bytes deterministically (a peer's data is `[peer.0 as u8; N]`)
/// so the oracle can verify any successful read against the same
/// rule.
///
/// Returns `(transports, source, dst_buffer)`. Each transport's
/// `bulk_get` will write into `dst_buffer`; the harness reads from
/// `dst_buffer` to check what landed.
pub(crate) fn make_pair(
    cfg: Rc<MercurySimCfg>,
    peers: Vec<PeerId>,
    page_size: u32,
    peer_data_len: usize,
) -> (
    HashMap<PeerId, MockTransport>,
    MockBulkSource,
    Rc<RefCell<Vec<u8>>>,
) {
    let bytes_by_peer: PeerBytes = Rc::new(RefCell::new(HashMap::new()));
    {
        let mut map = bytes_by_peer.borrow_mut();
        for p in &peers {
            map.insert(*p, vec![p.0 as u8; peer_data_len]);
        }
    }

    // Sized for a generous worst-case page count; tests can adjust by
    // resizing the returned buffer if needed.
    let dst_buffer = Rc::new(RefCell::new(vec![0u8; (page_size as usize) * 64]));

    let mut transports = HashMap::new();
    for p in &peers {
        transports.insert(
            *p,
            MockTransport::new(
                Rc::clone(&cfg),
                Rc::clone(&bytes_by_peer),
                *p,
                page_size,
                Rc::clone(&dst_buffer),
            ),
        );
    }

    // The mock server arbitrarily owns the first peer's identity. The
    // workload that uses this helper can construct additional sources
    // for peers that need their own identity by calling
    // `MockBulkSource::new` directly.
    let self_peer = *peers.first().expect("make_pair: at least one peer");
    let source = MockBulkSource::new(Rc::clone(&cfg), Rc::clone(&bytes_by_peer), self_peer);

    (transports, source, dst_buffer)
}

// =====================================================================
// Internal helpers
// =====================================================================

fn clamp01(p: f64) -> f64 {
    if p.is_nan() {
        0.0
    } else if p < 0.0 {
        0.0
    } else if p > 1.0 {
        1.0
    } else {
        p
    }
}

fn draw_latency(cfg: &MercurySimCfg) -> u32 {
    let lo = cfg.min_latency_yields;
    let hi = cfg.max_latency_yields.max(lo);
    if hi == 0 {
        0
    } else if lo == hi {
        lo
    } else {
        with_sim(|s| s.rng.gen_range(lo..=hi))
    }
}

// =====================================================================
// Tests
// =====================================================================

#[cfg(test)]
mod tests {
    use super::*;
    use crate::framework::executor::Executor;
    use std::sync::Arc;

    /// Drive a single future to completion under a fresh executor.
    fn drive<F, T>(seed: u64, f: F) -> T
    where
        F: Future<Output = T> + 'static,
        T: 'static,
    {
        let out: Rc<RefCell<Option<T>>> = Rc::new(RefCell::new(None));
        let out_w = Rc::clone(&out);
        let mut exec = Executor::new(seed);
        exec.spawn(async move {
            let v = f.await;
            *out_w.borrow_mut() = Some(v);
        });
        exec.run(1_000_000).expect("executor ran cleanly");
        out.borrow_mut().take().expect("future produced no value")
    }

    fn cfg_with(rate_forward: f64, rate_short: f64, rate_disc: f64) -> Rc<MercurySimCfg> {
        Rc::new(MercurySimCfg {
            min_latency_yields: 0,
            max_latency_yields: 2,
            forward_fault_rate: rate_forward,
            short_read_rate: rate_short,
            peer_disconnect_rate: rate_disc,
            capacity: u32::MAX,
            counters: Rc::new(RefCell::new(MercuryCounters::default())),
        })
    }

    #[test]
    fn mock_transport_basic_round_trip() {
        let page_size: u32 = 64;
        let peer = PeerId(7);
        let cfg = cfg_with(0.0, 0.0, 0.0);
        let counters = Rc::clone(&cfg.counters);
        let (transports, _source, dst) = make_pair(Rc::clone(&cfg), vec![peer], page_size, 4096);

        // Move the transport into the future.
        let transport = transports.into_iter().next().unwrap().1;
        let dst_for_check = Rc::clone(&dst);

        let req = MockReq { key: [0u8; 32] };
        let bulk = BulkRef {
            stripe: req.key(),
            offset: 128,
            len: 32,
        };
        let page = PageRef {
            page_idx: 1,
            offset: 0,
            len: 32,
        };

        let res = drive(0xC0FFEE, async move {
            transport.bulk_get(&req, bulk, page).await
        });
        res.expect("happy-path bulk_get should succeed");

        // Bytes copied are `[peer.0 as u8; 32]`.
        let buf = dst_for_check.borrow();
        let dst_off = (page.page_idx as usize) * (page_size as usize) + page.offset as usize;
        for b in &buf[dst_off..dst_off + 32] {
            assert_eq!(*b, peer.0 as u8, "unexpected dst byte");
        }
        let c = counters.borrow();
        assert_eq!(c.forwards_started, 1);
        assert_eq!(c.forwards_completed_ok, 1);
        assert_eq!(c.forwards_completed_err, 0);
        assert_eq!(c.bytes_pushed, 32);
        assert_eq!(c.current_in_flight, 0);
    }

    #[test]
    fn forward_fault_increments_err_counter() {
        let cfg = cfg_with(1.0, 0.0, 0.0);
        let counters = Rc::clone(&cfg.counters);
        let peer = PeerId(1);
        let (transports, _source, _dst) = make_pair(Rc::clone(&cfg), vec![peer], 64, 1024);
        let transport = transports.into_iter().next().unwrap().1;
        let req = MockReq { key: [0u8; 32] };
        let bulk = BulkRef {
            stripe: req.key(),
            offset: 0,
            len: 16,
        };
        let page = PageRef {
            page_idx: 0,
            offset: 0,
            len: 16,
        };
        let res = drive(1, async move { transport.bulk_get(&req, bulk, page).await });
        let err = res.expect_err("forward_fault_rate=1.0 must error");
        match err {
            BpError::Transport(_) => {}
            other => panic!("expected Transport error, got {other:?}"),
        }
        let c = counters.borrow();
        assert_eq!(c.forwards_completed_err, 1);
        assert_eq!(c.forwards_completed_ok, 0);
        assert_eq!(c.current_in_flight, 0);
    }

    #[test]
    fn short_read_returns_error() {
        let cfg = cfg_with(0.0, 1.0, 0.0);
        let counters = Rc::clone(&cfg.counters);
        let peer = PeerId(3);
        let (transports, _source, _dst) = make_pair(Rc::clone(&cfg), vec![peer], 64, 1024);
        let transport = transports.into_iter().next().unwrap().1;
        let req = MockReq { key: [0u8; 32] };
        let bulk = BulkRef {
            stripe: req.key(),
            offset: 0,
            len: 16,
        };
        let page = PageRef {
            page_idx: 0,
            offset: 0,
            len: 16,
        };
        let res = drive(2, async move { transport.bulk_get(&req, bulk, page).await });
        let err = res.expect_err("short_read_rate=1.0 must error");
        match err {
            BpError::Transport(e) => {
                let s = format!("{e}");
                assert!(s.contains("short read"), "unexpected message: {s}");
            }
            other => panic!("expected Transport error, got {other:?}"),
        }
        let c = counters.borrow();
        assert_eq!(c.fetches_short, 1, "client-side short read counted");
        assert_eq!(c.forwards_completed_err, 1);
        assert_eq!(c.current_in_flight, 0);
    }

    /// Sanity-check the trait object shape mirrors what
    /// `MercuryServer::new` will accept.
    #[test]
    fn bulk_source_trait_object_compiles() {
        let cfg = MercurySimCfg::new();
        let bytes: PeerBytes = Rc::new(RefCell::new(HashMap::new()));
        let src: Arc<dyn BulkSource<MockReq>> =
            Arc::new(MockBulkSource::new(cfg, bytes, PeerId(0)));
        let _ = Arc::clone(&src);
    }
}
