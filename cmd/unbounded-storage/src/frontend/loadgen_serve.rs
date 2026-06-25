// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Service-native synthetic read frontend.
//!
//! Unlike HTTP/S3, this frontend accepts no sockets. Each serving shard
//! owns a small set of synthetic workers that continuously issue normal
//! bufferpool reads through the same cache, p2p, storage, and origin path
//! as a client-facing GET. Prometheus service metrics are the benchmark
//! result surface.

use std::cell::Cell;
use std::future::Future;
use std::marker::PhantomData;
use std::pin::Pin;
use std::rc::Rc;
use std::task::{Context, Poll, Waker};
use std::time::Instant;

use rand::{RngCore, SeedableRng};
use rand_chacha::ChaCha8Rng;

use crate::bufferpool::{BufferPool, ReadStream, Req, StripePlan};
use crate::config::{FrontendSpec, frontend_spec};
use crate::frontend::FrontendError;
use crate::frontend::range::ResolvedRange;
use crate::metrics;
use crate::p2p::{RouteTableHandle, stripe_to_ring};
use crate::storage::{
    ObjectMetadata, OriginRef, StripeReq, SyntheticObjectId, object_hash, splitmix64,
    synthetic_matches_bytes, synthetic_object_id,
};

const DEFAULT_WORKERS: u32 = 1;
const DEFAULT_SEED: u64 = 0xBE_DE_CA_FE;
const DEFAULT_KEYSPACE_OBJECTS: u64 = 1_000_000;
const DEFAULT_ZIPF_EXPONENT: f64 = 1.1;

/// Synthetic frontend factory. Built once per [`FrontendSpec`]; holds
/// only immutable load-shaping configuration.
pub struct LoadgenFrontend {
    id: String,
    workers: u32,
    seed: u64,
    keyspace_objects: u64,
    expected_object_size_bytes: Option<u64>,
    read_bytes: u64,
    zipf_exponent: f64,
    verify: bool,
    remote_only: bool,
    fabric_only: bool,
    local_only: bool,
    skip_local_disk: bool,
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
        let zipf_exponent = loadgen.zipf_exponent.unwrap_or(DEFAULT_ZIPF_EXPONENT);
        if !zipf_exponent.is_finite() || zipf_exponent <= 0.0 {
            return Err(FrontendError::BadConfig(
                "loadgen zipf_exponent must be positive",
            ));
        }

        Ok(Self {
            id: spec.name.clone(),
            workers: loadgen.workers.unwrap_or(DEFAULT_WORKERS),
            seed: loadgen.seed.unwrap_or(DEFAULT_SEED),
            keyspace_objects: loadgen.keyspace_objects.unwrap_or(DEFAULT_KEYSPACE_OBJECTS),
            expected_object_size_bytes: loadgen.object_size_bytes,
            read_bytes: loadgen.read_bytes.unwrap_or(0),
            zipf_exponent,
            verify: loadgen.verify,
            remote_only: loadgen.remote_only,
            fabric_only: loadgen.fabric_only,
            local_only: loadgen.local_only,
            skip_local_disk: loadgen.skip_local_disk,
        })
    }

    pub fn id(&self) -> &str {
        &self.id
    }
}

/// Per-shard synthetic read driver, generic over the concrete pool type.
pub struct LoadgenDriver<P: BufferPool<Req = StripeReq> + 'static> {
    workers: Vec<LoadgenWorker>,
    waker: Waker,
    _marker: PhantomData<fn() -> P>,
}

impl<P: BufferPool<Req = StripeReq> + 'static> LoadgenDriver<P> {
    #[allow(clippy::too_many_arguments)]
    pub fn new(
        frontend: LoadgenFrontend,
        pool: Rc<P>,
        backend_id: String,
        cache_id: Option<String>,
        stripe_size: u64,
        page_size: usize,
        routes: RouteTableHandle,
        bypass: bool,
        shard_idx: u16,
        waker: Waker,
    ) -> Self {
        let worker_count = frontend.workers as usize;
        let cfg = Rc::new(LoadgenRun {
            frontend_id: frontend.id,
            backend_id,
            cache_id,
            stripe_size,
            page_size,
            bypass,
            shard_idx,
            seed: frontend.seed,
            expected_object_size_bytes: frontend.expected_object_size_bytes,
            read_bytes: frontend.read_bytes,
            sampler: ZipfSampler::new(frontend.keyspace_objects, frontend.zipf_exponent),
            verify: frontend.verify,
            routes,
            remote_only: frontend.remote_only,
            fabric_only: frontend.fabric_only,
            local_only: frontend.local_only,
            skip_local_disk: frontend.skip_local_disk,
        });
        let workers = (0..worker_count)
            .map(|worker_idx| {
                let rng = worker_rng(cfg.seed, cfg.shard_idx, worker_idx);
                let completed = Rc::new(Cell::new(false));
                LoadgenWorker {
                    completed: Rc::clone(&completed),
                    fut: loadgen_worker_loop(Rc::clone(&pool), Rc::clone(&cfg), rng, completed),
                }
            })
            .collect();
        Self {
            workers,
            waker,
            _marker: PhantomData,
        }
    }

    /// Poll every synthetic worker once. Each worker owns a long-lived
    /// loop future, so load runs until the frontend is removed or the
    /// shard shuts down.
    pub fn progress(&mut self) -> bool {
        let mut busy = false;
        let waker = self.waker.clone();
        let mut cx = Context::from_waker(&waker);
        for worker in &mut self.workers {
            worker.completed.set(false);
            let _ = worker.fut.as_mut().poll(&mut cx);
            if worker.completed.replace(false) {
                busy = true;
            }
        }
        busy
    }
}

struct LoadgenWorker {
    completed: Rc<Cell<bool>>,
    fut: Pin<Box<dyn Future<Output = ()>>>,
}

type WorkerRng = ChaCha8Rng;

struct LoadgenRun {
    frontend_id: String,
    backend_id: String,
    cache_id: Option<String>,
    stripe_size: u64,
    page_size: usize,
    bypass: bool,
    shard_idx: u16,
    seed: u64,
    expected_object_size_bytes: Option<u64>,
    read_bytes: u64,
    sampler: ZipfSampler,
    verify: bool,
    routes: RouteTableHandle,
    remote_only: bool,
    fabric_only: bool,
    local_only: bool,
    skip_local_disk: bool,
}

fn loadgen_worker_loop<P: BufferPool<Req = StripeReq> + 'static>(
    pool: Rc<P>,
    cfg: Rc<LoadgenRun>,
    mut rng: WorkerRng,
    completed: Rc<Cell<bool>>,
) -> Pin<Box<dyn Future<Output = ()>>> {
    Box::pin(async move {
        loop {
            let object = sample_object(&cfg, &mut rng);
            let start = Instant::now();
            let result = read_generated_object(&pool, &cfg, object).await;
            let (status, bytes) = match result {
                Ok(bytes) => (200, bytes),
                Err(()) => (500, 0),
            };
            metrics::frontend_request(
                &cfg.frontend_id,
                "GET",
                status,
                bytes,
                start.elapsed().as_secs_f64(),
            );
            completed.set(true);
            YieldOnce::new().await;
        }
    })
}

fn sample_object(cfg: &LoadgenRun, rng: &mut WorkerRng) -> SyntheticObjectId {
    const MAX_REMOTE_SAMPLE_ATTEMPTS: usize = 1024;

    let mut fallback = None;
    for _ in 0..MAX_REMOTE_SAMPLE_ATTEMPTS {
        let object = cfg.sampler.sample(cfg.seed, rng);
        if fallback.is_none() {
            fallback = Some(object);
        }
        if sample_object_matches_route_mode(cfg, object) {
            return object;
        }
    }

    fallback.expect("sampler loop always records first sample")
}

fn sample_object_matches_route_mode(cfg: &LoadgenRun, object: SyntheticObjectId) -> bool {
    let remote = first_stripe_routes_remote(cfg, object);
    match (cfg.remote_only, cfg.local_only) {
        (true, true) => false,
        (true, false) => remote,
        (false, true) => !remote,
        (false, false) => true,
    }
}

fn first_stripe_routes_remote(cfg: &LoadgenRun, object: SyntheticObjectId) -> bool {
    if cfg.bypass || cfg.cache_id.is_none() {
        return false;
    }

    let object_id = synthetic_object_id(object.seed, object.ordinal);
    let req = request_from_origin(OriginRef::new(&cfg.backend_id, &object_id, 0), cfg);
    let Some(route) = cfg.routes.route_for_req(&req) else {
        return false;
    };

    route.fingers.next_hop(stripe_to_ring(req.key())).is_some()
}

struct YieldOnce {
    yielded: bool,
}

impl YieldOnce {
    fn new() -> Self {
        Self { yielded: false }
    }
}

impl Future for YieldOnce {
    type Output = ();

    fn poll(mut self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<Self::Output> {
        if self.yielded {
            Poll::Ready(())
        } else {
            self.yielded = true;
            cx.waker().wake_by_ref();
            Poll::Pending
        }
    }
}

async fn read_generated_object<P: BufferPool<Req = StripeReq>>(
    pool: &Rc<P>,
    cfg: &LoadgenRun,
    object: SyntheticObjectId,
) -> Result<u64, ()> {
    let object_id = synthetic_object_id(object.seed, object.ordinal);
    let descriptor = if cfg.verify || cfg.expected_object_size_bytes.is_none() {
        let actual = read_object_length(pool, cfg, &object_id).await?;
        if cfg
            .expected_object_size_bytes
            .is_some_and(|expected| actual.length != expected)
        {
            return Err(());
        }
        actual
    } else {
        ObjectDescriptor {
            length: cfg
                .expected_object_size_bytes
                .expect("object_size_bytes presence checked"),
            data_identity: object_id.clone(),
        }
    };
    let read_len = if cfg.read_bytes == 0 {
        descriptor.length
    } else {
        cfg.read_bytes.min(descriptor.length)
    };
    let range = ResolvedRange {
        start: 0,
        end: read_len,
    };
    read_body(
        pool,
        cfg,
        object,
        &object_id,
        &descriptor.data_identity,
        range,
    )
    .await
}

async fn read_object_length<P: BufferPool<Req = StripeReq>>(
    pool: &Rc<P>,
    cfg: &LoadgenRun,
    object_id: &str,
) -> Result<ObjectDescriptor, ()> {
    let origin_ref = OriginRef::metadata_entry(&cfg.backend_id, object_id);
    let req = request_from_origin(origin_ref, cfg);
    let mut rs: ReadStream = pool
        .read(&req, 0, cfg.page_size as u64)
        .await
        .map_err(|_| ())?;
    let page = rs.next_page().await.ok_or(())?.map_err(|_| ())?;
    let meta = ObjectMetadata::decode(page.as_slice()).map_err(|_| ())?;
    if meta.is_not_found() {
        return Err(());
    }
    Ok(ObjectDescriptor {
        length: meta.length,
        data_identity: meta.data_identity.unwrap_or_else(|| object_id.to_string()),
    })
}

struct ObjectDescriptor {
    length: u64,
    data_identity: String,
}

async fn read_body<P: BufferPool<Req = StripeReq>>(
    pool: &Rc<P>,
    cfg: &LoadgenRun,
    object: SyntheticObjectId,
    object_id: &str,
    data_identity: &str,
    range: ResolvedRange,
) -> Result<u64, ()> {
    let plans = stripe_plans(range, cfg, object_id, data_identity);
    let mut bytes = 0u64;
    let mut checked_offset = range.start;
    let mut rs = pool.read_pipelined(plans, usize::MAX).map_err(|_| ())?;
    while let Some(page) = rs.next_page().await {
        let page = page.map_err(|_| ())?;
        if cfg.verify && !synthetic_matches_bytes(object, checked_offset, page.as_slice()) {
            return Err(());
        }
        checked_offset = checked_offset.wrapping_add(page.len() as u64);
        bytes += page.len() as u64;
        drop(page);
    }
    Ok(bytes)
}

fn stripe_plans(
    range: ResolvedRange,
    cfg: &LoadgenRun,
    object_id: &str,
    data_identity: &str,
) -> Vec<StripePlan<StripeReq>> {
    if cfg.stripe_size == 0 || range.is_empty() {
        return Vec::new();
    }

    let first = range.start / cfg.stripe_size;
    let last = (range.end - 1) / cfg.stripe_size;
    let mut plans = Vec::with_capacity((last - first + 1) as usize);
    for stripe_idx in first..=last {
        let stripe_start = stripe_idx * cfg.stripe_size;
        let start = range.start.max(stripe_start);
        let end = range.end.min(stripe_start + cfg.stripe_size);
        let origin_ref = OriginRef::new(&cfg.backend_id, object_id, stripe_idx)
            .with_data_identity(data_identity.to_string());
        plans.push(StripePlan {
            req: request_from_origin(origin_ref, cfg),
            intra_offset: start - stripe_start,
            intra_len: end - start,
        });
    }
    plans
}

fn request_from_origin(origin_ref: OriginRef, cfg: &LoadgenRun) -> StripeReq {
    let key = cfg
        .cache_id
        .as_deref()
        .map(|cache_id| origin_ref.stripe_key_for_cache(cache_id))
        .unwrap_or_else(|| origin_ref.stripe_key());
    StripeReq::new(key)
        .with_origin(origin_ref)
        .with_cache_id(cfg.cache_id.clone())
        .with_bypass(cfg.bypass)
        .with_fabric_only(cfg.fabric_only)
        .with_skip_local_disk(cfg.skip_local_disk)
}

#[derive(Clone, Copy)]
struct ZipfSampler {
    keyspace_objects: u64,
    exponent: f64,
    t: f64,
    q: f64,
}

impl ZipfSampler {
    fn new(keyspace_objects: u64, exponent: f64) -> Self {
        let keyspace_objects = keyspace_objects.max(1);
        let n = keyspace_objects as f64;
        let (t, q) = if exponent == 1.0 {
            (1.0 + n.ln(), 0.0)
        } else {
            let one_minus_s = 1.0 - exponent;
            (
                (n.powf(one_minus_s) - exponent) / one_minus_s,
                1.0 / one_minus_s,
            )
        };

        Self {
            keyspace_objects,
            exponent,
            t,
            q,
        }
    }

    fn sample(&self, seed: u64, rng: &mut WorkerRng) -> SyntheticObjectId {
        let rank = self.sample_rank(rng);
        let ordinal = rank - 1;
        SyntheticObjectId {
            seed,
            ordinal,
            hash: object_hash(seed, ordinal),
        }
    }

    fn sample_rank(&self, rng: &mut WorkerRng) -> u64 {
        if self.keyspace_objects == 1 {
            return 1;
        }

        loop {
            let inv = self.inverse_cdf(unit_f64(rng));
            let rank = (inv + 1.0).floor();
            let mut accept = rank.powf(-self.exponent);
            if rank > 1.0 {
                accept *= inv.powf(self.exponent);
            }

            if unit_f64(rng) < accept {
                return (rank as u64).clamp(1, self.keyspace_objects);
            }
        }
    }

    fn inverse_cdf(&self, p: f64) -> f64 {
        let scaled = p * self.t;
        if scaled <= 1.0 {
            scaled
        } else if self.exponent == 1.0 {
            (scaled - 1.0).exp()
        } else {
            (scaled.mul_add(1.0 - self.exponent, self.exponent)).powf(self.q)
        }
    }
}

fn worker_rng(seed: u64, shard_idx: u16, worker_idx: usize) -> WorkerRng {
    let mixed = splitmix64(seed ^ ((shard_idx as u64) << 48) ^ worker_idx as u64);
    WorkerRng::seed_from_u64(mixed)
}

fn unit_f64(rng: &mut WorkerRng) -> f64 {
    const SCALE: f64 = 1.0 / ((1u64 << 53) as f64);
    ((rng.next_u64() >> 11) as f64) * SCALE
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::pin::Pin;
    use std::task::{RawWaker, RawWakerVTable, Waker};

    use crate::bufferpool::{PageRef, Req};
    use crate::config::{HttpFrontendConfig, LoadgenFrontendConfig};
    use crate::p2p::{FingerTable, FingerTableConfig, NodeId, PeerEntry, RingId, TopologyTags};

    fn noop_waker() -> Waker {
        fn no_op(_: *const ()) {}
        fn clone(_: *const ()) -> RawWaker {
            RawWaker::new(std::ptr::null(), &VTABLE)
        }

        static VTABLE: RawWakerVTable = RawWakerVTable::new(clone, no_op, no_op, no_op);
        // SAFETY: the vtable's fns are all no-ops over a null data pointer.
        unsafe { Waker::from_raw(RawWaker::new(std::ptr::null(), &VTABLE)) }
    }

    fn block_on<F: Future>(fut: F) -> F::Output {
        let waker = noop_waker();
        let mut cx = Context::from_waker(&waker);
        let mut fut = Box::pin(fut);
        loop {
            match Future::poll(Pin::as_mut(&mut fut), &mut cx) {
                Poll::Ready(v) => return v,
                Poll::Pending => std::thread::yield_now(),
            }
        }
    }

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
            stripe_size: 4096,
            page_size: 4096,
            bypass: false,
            shard_idx: 2,
            seed: 7,
            expected_object_size_bytes: None,
            read_bytes: 0,
            sampler: ZipfSampler::new(5, DEFAULT_ZIPF_EXPONENT),
            verify: false,
            routes: RouteTableHandle::empty(),
            remote_only: false,
            fabric_only: false,
            local_only: false,
            skip_local_disk: false,
        }
    }

    fn route_handle(local_ring: u64, peer_ring: u64) -> RouteTableHandle {
        let local = PeerEntry {
            node: NodeId(1),
            ring: RingId(local_ring),
            tags: TopologyTags(Vec::new()),
        };
        let peer = PeerEntry {
            node: NodeId(2),
            ring: RingId(peer_ring),
            tags: TopologyTags(Vec::new()),
        };
        let fingers = std::sync::Arc::new(FingerTable::build(
            local,
            &[peer],
            FingerTableConfig::with_k(4),
        ));
        let node_to_peer = std::sync::Arc::new(std::collections::HashMap::from([(
            NodeId(2),
            crate::fabric::PeerId(2),
        )]));
        let mut routes = std::collections::HashMap::new();
        routes.insert(
            "cache".to_string(),
            crate::p2p::RoutingSnapshot {
                fingers,
                node_to_peer,
            },
        );

        RouteTableHandle::new(routes)
    }

    fn find_object_with_remote_route(cfg: &LoadgenRun, want_remote: bool) -> SyntheticObjectId {
        for ordinal in 0..100_000 {
            let object = SyntheticObjectId {
                seed: cfg.seed,
                ordinal,
                hash: object_hash(cfg.seed, ordinal),
            };
            if first_stripe_routes_remote(cfg, object) == want_remote {
                return object;
            }
        }

        panic!("could not find object with desired route");
    }

    struct MetadataProbePool {
        metadata_reads: Cell<u32>,
    }

    impl MetadataProbePool {
        fn new() -> Self {
            Self {
                metadata_reads: Cell::new(0),
            }
        }
    }

    impl BufferPool for MetadataProbePool {
        type Req = StripeReq;

        async fn read<'p>(
            &'p self,
            _req: &'p Self::Req,
            _offset: u64,
            _len: u64,
        ) -> Result<ReadStream<'p>, crate::bufferpool::Error> {
            self.metadata_reads.set(self.metadata_reads.get() + 1);
            Err(crate::bufferpool::Error::from("metadata probe"))
        }

        fn read_windowed<'p>(
            &'p self,
            _req: &'p Self::Req,
            _offset: u64,
            _len: u64,
            _window: usize,
        ) -> Result<crate::bufferpool::WindowedRead<'p>, crate::bufferpool::Error> {
            unimplemented!()
        }

        fn read_pipelined<'p>(
            &'p self,
            stripes: Vec<StripePlan<Self::Req>>,
            _window: usize,
        ) -> Result<crate::bufferpool::PipelinedRead<'p>, crate::bufferpool::Error> {
            assert!(
                stripes.is_empty(),
                "zero-length object should not read data"
            );
            Err(crate::bufferpool::Error::from(
                "end test after metadata skip",
            ))
        }
    }

    #[test]
    fn frontend_defaults_load_shape() {
        let frontend = LoadgenFrontend::from_spec(&loadgen_spec()).unwrap();
        assert_eq!(frontend.id(), "lg");
        assert_eq!(frontend.workers, DEFAULT_WORKERS);
        assert_eq!(frontend.seed, DEFAULT_SEED);
        assert_eq!(frontend.keyspace_objects, DEFAULT_KEYSPACE_OBJECTS);
        assert_eq!(frontend.expected_object_size_bytes, None);
        assert_eq!(frontend.read_bytes, 0);
        assert_eq!(frontend.zipf_exponent, DEFAULT_ZIPF_EXPONENT);
        assert!(!frontend.verify);
        assert!(!frontend.remote_only);
        assert!(!frontend.fabric_only);
        assert!(!frontend.local_only);
        assert!(!frontend.skip_local_disk);
    }

    #[test]
    fn explicit_zeroes_are_preserved() {
        let mut spec = loadgen_spec();
        spec.config = Some(frontend_spec::Config::Loadgen(LoadgenFrontendConfig {
            workers: Some(0),
            seed: Some(0),
            keyspace_objects: Some(0),
            object_size_bytes: Some(0),
            read_bytes: Some(0),
            zipf_exponent: Some(1.0),
            verify: false,
            remote_only: false,
            fabric_only: false,
            local_only: false,
            skip_local_disk: false,
        }));
        let frontend = LoadgenFrontend::from_spec(&spec).unwrap();
        assert_eq!(frontend.workers, 0);
        assert_eq!(frontend.seed, 0);
        assert_eq!(frontend.keyspace_objects, 0);
        assert_eq!(frontend.expected_object_size_bytes, Some(0));
        assert_eq!(frontend.read_bytes, 0);
    }

    #[test]
    fn remote_only_flag_loads_from_spec() {
        let mut spec = loadgen_spec();
        spec.config = Some(frontend_spec::Config::Loadgen(LoadgenFrontendConfig {
            remote_only: true,
            ..LoadgenFrontendConfig::default()
        }));

        let frontend = LoadgenFrontend::from_spec(&spec).unwrap();
        assert!(frontend.remote_only);
    }

    #[test]
    fn fabric_only_flag_loads_from_spec() {
        let mut spec = loadgen_spec();
        spec.config = Some(frontend_spec::Config::Loadgen(LoadgenFrontendConfig {
            fabric_only: true,
            ..LoadgenFrontendConfig::default()
        }));

        let frontend = LoadgenFrontend::from_spec(&spec).unwrap();
        assert!(frontend.fabric_only);
    }

    #[test]
    fn local_only_flag_loads_from_spec() {
        let mut spec = loadgen_spec();
        spec.config = Some(frontend_spec::Config::Loadgen(LoadgenFrontendConfig {
            local_only: true,
            ..LoadgenFrontendConfig::default()
        }));

        let frontend = LoadgenFrontend::from_spec(&spec).unwrap();
        assert!(frontend.local_only);
    }

    #[test]
    fn skip_local_disk_flag_loads_from_spec() {
        let mut spec = loadgen_spec();
        spec.config = Some(frontend_spec::Config::Loadgen(LoadgenFrontendConfig {
            skip_local_disk: true,
            ..LoadgenFrontendConfig::default()
        }));

        let frontend = LoadgenFrontend::from_spec(&spec).unwrap();
        assert!(frontend.skip_local_disk);
    }

    #[test]
    fn invalid_zipf_exponent_is_rejected() {
        let mut spec = loadgen_spec();
        spec.config = Some(frontend_spec::Config::Loadgen(LoadgenFrontendConfig {
            zipf_exponent: Some(0.0),
            ..LoadgenFrontendConfig::default()
        }));

        assert!(matches!(
            LoadgenFrontend::from_spec(&spec),
            Err(FrontendError::BadConfig(_))
        ));
    }

    #[test]
    fn zipf_sampler_is_deterministic_and_bounded() {
        let sampler = ZipfSampler::new(5, DEFAULT_ZIPF_EXPONENT);
        let mut first = worker_rng(7, 2, 3);
        let mut second = worker_rng(7, 2, 3);

        for _ in 0..100 {
            let a = sampler.sample(7, &mut first);
            let b = sampler.sample(7, &mut second);
            assert_eq!(a, b);
            assert_eq!(a.seed, 7);
            assert!(a.ordinal < 5);
        }
    }

    #[test]
    fn zipf_sampler_can_reach_tail_rank() {
        let sampler = ZipfSampler::new(10, 1.0);
        let mut rng = worker_rng(7, 2, 3);
        let mut saw_tail = false;
        for _ in 0..100_000 {
            saw_tail |= sampler.sample_rank(&mut rng) == 10;
        }

        assert!(
            saw_tail,
            "finite Zipf sampler should be able to sample rank n"
        );
    }

    #[test]
    fn zipf_sampler_rank_counts_decrease_with_rank() {
        let sampler = ZipfSampler::new(5, DEFAULT_ZIPF_EXPONENT);
        let mut rng = worker_rng(7, 2, 3);
        let mut counts = [0usize; 5];
        for _ in 0..100_000 {
            counts[(sampler.sample_rank(&mut rng) - 1) as usize] += 1;
        }

        for pair in counts.windows(2) {
            assert!(
                pair[0] > pair[1],
                "rank counts were not descending: {counts:?}"
            );
        }
    }

    #[test]
    fn request_carries_origin_chain_and_bypass() {
        let mut cfg = run_cfg();
        cfg.bypass = true;
        cfg.fabric_only = true;
        cfg.skip_local_disk = true;
        let origin = OriginRef::new("fake", "obj", 3);
        let key = origin.stripe_key_for_cache("cache");
        let req = request_from_origin(origin, &cfg);
        assert_eq!(req.key(), key);
        assert_eq!(req.origin().unwrap().backend_id, "fake");
        assert_eq!(req.cache_id(), Some("cache"));
        assert!(crate::bufferpool::Req::bypass(&req));
        assert!(req.fabric_only());
        assert!(req.skip_local_disk());
    }

    #[test]
    fn route_mode_predicate_selects_local_or_remote() {
        let mut cfg = run_cfg();
        cfg.routes = route_handle(100, u64::MAX / 2);

        let remote = find_object_with_remote_route(&cfg, true);
        let local = find_object_with_remote_route(&cfg, false);

        cfg.remote_only = true;
        assert!(sample_object_matches_route_mode(&cfg, remote));
        assert!(!sample_object_matches_route_mode(&cfg, local));

        cfg.remote_only = false;
        cfg.local_only = true;
        assert!(!sample_object_matches_route_mode(&cfg, remote));
        assert!(sample_object_matches_route_mode(&cfg, local));

        cfg.remote_only = true;
        assert!(!sample_object_matches_route_mode(&cfg, remote));
        assert!(!sample_object_matches_route_mode(&cfg, local));
    }

    #[test]
    fn remote_route_predicate_uses_route_table() {
        let mut cfg = run_cfg();
        cfg.routes = route_handle(100, u64::MAX / 2);

        let remote = find_object_with_remote_route(&cfg, true);
        let local = find_object_with_remote_route(&cfg, false);

        assert!(first_stripe_routes_remote(&cfg, remote));
        assert!(!first_stripe_routes_remote(&cfg, local));
    }

    #[test]
    fn remote_route_predicate_is_false_for_bypass_or_missing_cache() {
        let mut cfg = run_cfg();
        cfg.routes = route_handle(100, u64::MAX / 2);
        let remote = find_object_with_remote_route(&cfg, true);

        cfg.bypass = true;
        assert!(!first_stripe_routes_remote(&cfg, remote));

        cfg.bypass = false;
        cfg.cache_id = None;
        assert!(!first_stripe_routes_remote(&cfg, remote));
    }

    #[test]
    fn configured_object_size_skips_metadata_read() {
        let mut cfg = run_cfg();
        cfg.expected_object_size_bytes = Some(0);
        let pool = Rc::new(MetadataProbePool::new());
        let object = SyntheticObjectId {
            seed: cfg.seed,
            ordinal: 0,
            hash: object_hash(cfg.seed, 0),
        };

        assert_eq!(
            block_on(read_generated_object(&pool, &cfg, object)),
            Err(())
        );
        assert_eq!(pool.metadata_reads.get(), 0);
    }

    #[test]
    fn configured_object_size_with_verify_reads_metadata() {
        let mut cfg = run_cfg();
        cfg.expected_object_size_bytes = Some(0);
        cfg.verify = true;
        let pool = Rc::new(MetadataProbePool::new());
        let object = SyntheticObjectId {
            seed: cfg.seed,
            ordinal: 0,
            hash: object_hash(cfg.seed, 0),
        };

        assert_eq!(
            block_on(read_generated_object(&pool, &cfg, object)),
            Err(())
        );
        assert_eq!(pool.metadata_reads.get(), 1);
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
        assert!(
            crate::frontend::range::stripe_set(crate::frontend::range::full_object(0), 4096)
                .is_empty()
        );
    }
}
