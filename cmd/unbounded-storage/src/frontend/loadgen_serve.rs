// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Service-native synthetic read frontend.
//!
//! Unlike HTTP/S3, this frontend accepts no sockets. Each serving shard
//! owns a small set of synthetic workers that continuously issue normal
//! bufferpool reads through the same cache, p2p, storage, and origin path
//! as a client-facing GET. Prometheus service metrics are the benchmark
//! result surface.

use std::future::Future;
use std::pin::Pin;
use std::rc::Rc;
use std::task::{Context, Waker};
use std::time::Instant;

use crate::bufferpool::{BufferPool, ReadStream, StripePlan};
use crate::config::{FrontendSpec, frontend_spec};
use crate::frontend::FrontendError;
use crate::frontend::range::{ResolvedRange, stripe_set};
use crate::metrics;
use crate::p2p::{RouteTableHandle, stripe_to_ring};
use crate::runtime::noop_waker;
use crate::storage::{ObjectMetadata, OriginRef, StripeReq};

const DEFAULT_WORKERS: u32 = 1;
const DEFAULT_SEED: u64 = 0xBE_DE_CA_FE;
const DEFAULT_OBJECT_COUNT: u64 = 1_000_000;

/// Synthetic frontend factory. Built once per [`FrontendSpec`]; holds
/// only immutable load-shaping configuration.
pub struct LoadgenFrontend {
    id: String,
    workers: u32,
    seed: u64,
    object_count: u64,
    read_bytes: u64,
    verify: bool,
    fixed_object_size_bytes: Option<u64>,
    require_remote_peer: bool,
    warmup_operations: u64,
}

impl LoadgenFrontend {
    pub fn from_spec(spec: &FrontendSpec) -> Result<Self, FrontendError> {
        let loadgen = match spec.config.as_ref() {
            Some(frontend_spec::Config::Loadgen(cfg)) => cfg.clone(),
            _ => {
                return Err(FrontendError::UnsupportedKind(
                    "non-loadgen frontend config",
                ));
            }
        };
        Ok(Self {
            id: spec.name.clone(),
            workers: loadgen.workers.unwrap_or(DEFAULT_WORKERS),
            seed: loadgen.seed.unwrap_or(DEFAULT_SEED),
            object_count: loadgen.object_count.unwrap_or(DEFAULT_OBJECT_COUNT),
            read_bytes: loadgen.read_bytes.unwrap_or(0),
            verify: loadgen.verify,
            fixed_object_size_bytes: loadgen.fixed_object_size_bytes,
            require_remote_peer: loadgen.require_remote_peer.unwrap_or(false),
            warmup_operations: loadgen.warmup_operations.unwrap_or(0),
        })
    }

    pub fn id(&self) -> &str {
        &self.id
    }
}

/// Per-shard synthetic read driver, generic over the concrete pool type.
pub struct LoadgenDriver<P: BufferPool<Req = StripeReq> + 'static> {
    workers: Vec<LoadgenWorker>,
    cfg: Rc<LoadgenRun>,
    pool: Rc<P>,
    waker: Waker,
}

impl<P: BufferPool<Req = StripeReq> + 'static> LoadgenDriver<P> {
    #[allow(clippy::too_many_arguments)]
    pub fn new(
        frontend: LoadgenFrontend,
        pool: Rc<P>,
        backend_id: String,
        cache_id: Option<String>,
        neighborhood_id: Option<String>,
        stripe_size: u64,
        page_size: usize,
        bypass: bool,
        shard_idx: u16,
        routes: RouteTableHandle,
    ) -> Self {
        let worker_count = frontend.workers as usize;
        let cfg = Rc::new(LoadgenRun {
            frontend_id: frontend.id,
            backend_id,
            cache_id,
            neighborhood_id,
            stripe_size,
            page_size,
            bypass,
            shard_idx,
            seed: frontend.seed,
            object_count: frontend.object_count,
            read_bytes: frontend.read_bytes,
            verify: frontend.verify,
            fixed_object_size_bytes: frontend.fixed_object_size_bytes,
            require_remote_peer: frontend.require_remote_peer,
            warmup_operations: frontend.warmup_operations,
            routes,
        });
        let workers = (0..worker_count)
            .map(|worker_idx| LoadgenWorker {
                worker_idx,
                seq: 0,
                fut: loadgen_op(Rc::clone(&pool), Rc::clone(&cfg), worker_idx, 0),
            })
            .collect();
        Self {
            workers,
            cfg,
            pool,
            waker: noop_waker(),
        }
    }

    /// Poll every synthetic worker once. A completed operation is
    /// immediately replaced with that worker's next operation, so load
    /// runs until the frontend is removed or the shard shuts down.
    pub fn progress(&mut self) -> bool {
        let mut busy = false;
        let waker = self.waker.clone();
        let mut cx = Context::from_waker(&waker);
        for worker in &mut self.workers {
            if worker.fut.as_mut().poll(&mut cx).is_ready() {
                busy = true;
                worker.seq = worker.seq.wrapping_add(1);
                worker.fut = loadgen_op(
                    Rc::clone(&self.pool),
                    Rc::clone(&self.cfg),
                    worker.worker_idx,
                    worker.seq,
                );
            }
        }
        busy
    }
}

struct LoadgenWorker {
    worker_idx: usize,
    seq: u64,
    fut: Pin<Box<dyn Future<Output = ()>>>,
}

struct LoadgenRun {
    frontend_id: String,
    backend_id: String,
    cache_id: Option<String>,
    neighborhood_id: Option<String>,
    stripe_size: u64,
    page_size: usize,
    bypass: bool,
    shard_idx: u16,
    seed: u64,
    object_count: u64,
    read_bytes: u64,
    verify: bool,
    fixed_object_size_bytes: Option<u64>,
    require_remote_peer: bool,
    warmup_operations: u64,
    routes: RouteTableHandle,
}

fn loadgen_op<P: BufferPool<Req = StripeReq> + 'static>(
    pool: Rc<P>,
    cfg: Rc<LoadgenRun>,
    worker_idx: usize,
    seq: u64,
) -> Pin<Box<dyn Future<Output = ()>>> {
    Box::pin(async move {
        let start = Instant::now();
        let result = read_generated_object(&pool, &cfg, worker_idx, seq).await;
        let (status, bytes) = match result {
            Ok(bytes) => (200, bytes),
            Err(()) => (500, 0),
        };
        if seq >= cfg.warmup_operations {
            metrics::frontend_request(
                &cfg.frontend_id,
                "GET",
                status,
                bytes,
                start.elapsed().as_secs_f64(),
            );
        }
    })
}

async fn read_generated_object<P: BufferPool<Req = StripeReq>>(
    pool: &Rc<P>,
    cfg: &LoadgenRun,
    worker_idx: usize,
    seq: u64,
) -> Result<u64, ()> {
    let object_seq = object_seq_for_op(cfg, seq);
    let object_id = object_id_for_seq(cfg, worker_idx, object_seq);
    let object_len = match cfg.fixed_object_size_bytes {
        Some(size) => size,
        None => read_object_length(pool, cfg, &object_id).await?,
    };
    let read_len = if cfg.read_bytes == 0 {
        object_len
    } else {
        cfg.read_bytes.min(object_len)
    };
    let range = ResolvedRange {
        start: 0,
        end: read_len,
    };
    read_body(pool, cfg, &object_id, range).await
}

fn object_seq_for_op(cfg: &LoadgenRun, seq: u64) -> u64 {
    if seq >= cfg.warmup_operations {
        seq - cfg.warmup_operations
    } else {
        seq
    }
}

async fn read_object_length<P: BufferPool<Req = StripeReq>>(
    pool: &Rc<P>,
    cfg: &LoadgenRun,
    object_id: &str,
) -> Result<u64, ()> {
    let origin_ref = OriginRef::metadata_entry(&cfg.backend_id, object_id);
    let req = request_from_origin(origin_ref, cfg);
    let mut rs: ReadStream = pool
        .read(&req, 0, cfg.page_size as u64)
        .await
        .map_err(|_| ())?;
    let page = rs.next_page().await.ok_or(())?.map_err(|_| ())?;
    let meta = ObjectMetadata::decode(page.as_slice()).map_err(|_| ())?;
    Ok(meta.length)
}

async fn read_body<P: BufferPool<Req = StripeReq>>(
    pool: &Rc<P>,
    cfg: &LoadgenRun,
    object_id: &str,
    range: ResolvedRange,
) -> Result<u64, ()> {
    let plans: Vec<StripePlan<StripeReq>> = stripe_set(range, cfg.stripe_size)
        .into_iter()
        .map(|slice| {
            let origin_ref = OriginRef::new(&cfg.backend_id, object_id, slice.stripe_idx);
            StripePlan {
                req: request_from_origin(origin_ref, cfg),
                intra_offset: slice.intra_offset,
                intra_len: slice.intra_len,
            }
        })
        .collect();
    let mut bytes = 0u64;
    let mut rs = pool.read_pipelined(plans, usize::MAX).map_err(|_| ())?;
    while let Some(page) = rs.next_page().await {
        let page = page.map_err(|_| ())?;
        if cfg.verify && page.as_slice().iter().any(|b| *b != 0) {
            return Err(());
        }
        bytes += page.len() as u64;
        drop(page);
    }
    Ok(bytes)
}

fn request_from_origin(origin_ref: OriginRef, cfg: &LoadgenRun) -> StripeReq {
    StripeReq::new(origin_ref.stripe_key())
        .with_origin(origin_ref)
        .with_chain(cfg.cache_id.clone(), cfg.neighborhood_id.clone())
        .with_bypass(cfg.bypass)
}

fn object_id_for_seq(cfg: &LoadgenRun, worker_idx: usize, seq: u64) -> String {
    if cfg.require_remote_peer {
        return remote_filtered_object_id(cfg, worker_idx, seq);
    }

    object_id(
        &cfg.frontend_id,
        cfg.shard_idx,
        worker_idx,
        cfg.seed,
        cfg.object_count,
        seq,
    )
}

fn remote_filtered_object_id(cfg: &LoadgenRun, worker_idx: usize, seq: u64) -> String {
    let object_len = cfg
        .fixed_object_size_bytes
        .expect("remote-filtered loadgen requires fixed object size");
    let read_len = if cfg.read_bytes == 0 {
        object_len
    } else {
        cfg.read_bytes.min(object_len)
    };
    let stripes = stripe_set(
        ResolvedRange {
            start: 0,
            end: read_len,
        },
        cfg.stripe_size,
    );
    let route = cfg
        .neighborhood_id
        .as_deref()
        .and_then(|id| cfg.routes.route(id))
        .expect("remote-filtered loadgen requires a neighborhood route");

    let mut ordinal = cfg.seed.wrapping_add(seq);
    loop {
        let object_id = object_id_with_ordinal(&cfg.frontend_id, cfg.shard_idx, worker_idx, ordinal);
        if stripes.iter().all(|slice| {
            let origin_ref = OriginRef::new(&cfg.backend_id, &object_id, slice.stripe_idx);
            let stripe_ring = stripe_to_ring(origin_ref.stripe_key());
            !route.fingers.owns(stripe_ring)
        }) {
            return object_id;
        }
        ordinal = ordinal.wrapping_add(1);
    }
}

fn object_id(
    frontend_id: &str,
    shard_idx: u16,
    worker_idx: usize,
    seed: u64,
    object_count: u64,
    seq: u64,
) -> String {
    let ordinal = seed.wrapping_add(seq) % object_count.max(1);
    object_id_with_ordinal(frontend_id, shard_idx, worker_idx, ordinal)
}

fn object_id_with_ordinal(
    frontend_id: &str,
    shard_idx: u16,
    worker_idx: usize,
    ordinal: u64,
) -> String {
    format!("/__loadgen/{frontend_id}/{shard_idx}/{worker_idx}/{ordinal}")
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::bufferpool::Req;
    use crate::config::{HttpFrontendConfig, LoadgenFrontendConfig};
    use crate::p2p::PeerEntry;

    fn loadgen_spec() -> FrontendSpec {
        FrontendSpec {
            name: "lg".to_string(),
            source: "cache".to_string(),
            config: Some(frontend_spec::Config::Loadgen(
                LoadgenFrontendConfig::default(),
            )),
        }
    }

    fn run_cfg() -> LoadgenRun {
        LoadgenRun {
            frontend_id: "lg".to_string(),
            backend_id: "fake".to_string(),
            cache_id: Some("cache".to_string()),
            neighborhood_id: Some("n".to_string()),
            stripe_size: 4096,
            page_size: 4096,
            bypass: false,
            shard_idx: 2,
            seed: 7,
            object_count: 5,
            read_bytes: 0,
            verify: false,
            fixed_object_size_bytes: None,
            require_remote_peer: false,
            warmup_operations: 0,
            routes: RouteTableHandle::empty(),
        }
    }

    #[test]
    fn frontend_defaults_load_shape() {
        let frontend = LoadgenFrontend::from_spec(&loadgen_spec()).unwrap();
        assert_eq!(frontend.id(), "lg");
        assert_eq!(frontend.workers, DEFAULT_WORKERS);
        assert_eq!(frontend.seed, DEFAULT_SEED);
        assert_eq!(frontend.object_count, DEFAULT_OBJECT_COUNT);
        assert_eq!(frontend.read_bytes, 0);
        assert!(!frontend.verify);
        assert_eq!(frontend.fixed_object_size_bytes, None);
        assert!(!frontend.require_remote_peer);
        assert_eq!(frontend.warmup_operations, 0);
    }

    #[test]
    fn explicit_zeroes_are_preserved() {
        let mut spec = loadgen_spec();
        spec.config = Some(frontend_spec::Config::Loadgen(LoadgenFrontendConfig {
            workers: Some(0),
            seed: Some(0),
            object_count: Some(0),
            read_bytes: Some(0),
            verify: false,
            fixed_object_size_bytes: Some(0),
            require_remote_peer: Some(false),
            warmup_operations: Some(0),
        }));
        let frontend = LoadgenFrontend::from_spec(&spec).unwrap();
        assert_eq!(frontend.workers, 0);
        assert_eq!(frontend.seed, 0);
        assert_eq!(frontend.object_count, 0);
        assert_eq!(frontend.read_bytes, 0);
        assert_eq!(frontend.fixed_object_size_bytes, Some(0));
        assert!(!frontend.require_remote_peer);
        assert_eq!(frontend.warmup_operations, 0);
    }

    #[test]
    fn object_ids_are_deterministic_and_bounded() {
        assert_eq!(object_id("lg", 2, 3, 7, 5, 0), "/__loadgen/lg/2/3/2");
        assert_eq!(object_id("lg", 2, 3, 7, 5, 1), "/__loadgen/lg/2/3/3");
        assert_eq!(object_id("lg", 2, 3, 7, 0, 1), "/__loadgen/lg/2/3/0");
    }

    #[test]
    fn measured_sequence_replays_warmed_objects() {
        let mut cfg = run_cfg();
        cfg.warmup_operations = 3;

        assert_eq!(object_seq_for_op(&cfg, 0), 0);
        assert_eq!(object_seq_for_op(&cfg, 2), 2);
        assert_eq!(object_seq_for_op(&cfg, 3), 0);
        assert_eq!(object_seq_for_op(&cfg, 4), 1);
    }

    #[test]
    fn remote_filtered_object_ids_are_not_owned_locally() {
        let local = PeerEntry {
            node: crate::p2p::NodeId(1),
            ring: crate::p2p::node_to_ring(crate::p2p::NodeId(1)),
            tags: crate::p2p::TopologyTags::default(),
        };
        let peer = PeerEntry {
            node: crate::p2p::NodeId(2),
            ring: crate::p2p::node_to_ring(crate::p2p::NodeId(2)),
            tags: crate::p2p::TopologyTags::default(),
        };
        let fingers = std::sync::Arc::new(crate::p2p::FingerTable::build(
            local,
            &[peer],
            crate::p2p::FingerTableConfig { k: 8 },
        ));
        let mut routes = std::collections::HashMap::new();
        routes.insert(
            "n".to_string(),
            crate::p2p::RoutingSnapshot {
                fingers: fingers.clone(),
                node_to_peer: std::sync::Arc::new(std::collections::HashMap::new()),
            },
        );

        let mut cfg = run_cfg();
        cfg.fixed_object_size_bytes = Some(4096);
        cfg.require_remote_peer = true;
        cfg.routes = RouteTableHandle::new(routes);

        let object_id = object_id_for_seq(&cfg, 0, 0);
        let origin = OriginRef::new(&cfg.backend_id, object_id, 0);
        let key_ring = stripe_to_ring(origin.stripe_key());

        assert!(!fingers.owns(key_ring));
    }

    #[test]
    fn request_carries_origin_chain_and_bypass() {
        let mut cfg = run_cfg();
        cfg.bypass = true;
        let origin = OriginRef::new("fake", "obj", 3);
        let key = origin.stripe_key();
        let req = request_from_origin(origin, &cfg);
        assert_eq!(req.key(), key);
        assert_eq!(req.origin().unwrap().backend_id, "fake");
        assert_eq!(req.cache_id(), Some("cache"));
        assert_eq!(req.neighborhood_id(), Some("n"));
        assert!(crate::bufferpool::Req::bypass(&req));
    }

    #[test]
    fn non_loadgen_kind_is_rejected() {
        let mut spec = loadgen_spec();
        spec.config = Some(frontend_spec::Config::Http(HttpFrontendConfig {
            addr: "127.0.0.1:9000".to_string(),
            max_requests_per_connection: None,
        }));
        assert!(LoadgenFrontend::from_spec(&spec).is_err());
    }

    #[test]
    fn full_object_helper_is_available_for_zero_length() {
        assert!(stripe_set(crate::frontend::range::full_object(0), 4096).is_empty());
    }
}
