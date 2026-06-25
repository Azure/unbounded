// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Workload model and driver for hot-reload and lifecycle DST.

use std::cell::{Cell, RefCell};
use std::collections::VecDeque;
use std::future::{Future, poll_fn};
use std::path::PathBuf;
use std::pin::Pin;
use std::rc::Rc;
use std::sync::Arc;
use std::task::Poll;

use proptest::collection::vec;
use proptest::prelude::*;
use unbounded_storage::bufferpool::{Error, StripeKey};
use unbounded_storage::config::{
    ApplyError, BackendSpec, CacheSpec, Config, ConfigApplyTarget, ConfigController, ConfigDiff,
    DiskSpec, FileDiskConfig, FrontendSpec, HttpBackendConfig, HttpFrontendConfig, PeerSpec,
    TcpPeerConfig, backend_spec, disk_spec, frontend_spec, peer_spec, runtime_disks,
    runtime_projection,
};
use unbounded_storage::runtime::ShardLoop;
use unbounded_storage::storage::blockdev::MockDeviceConfig;
use unbounded_storage::storage::disks::{
    DiskChannelDirectory, DiskError, DiskRegistry, DiskTarget,
};
use unbounded_storage::storage::{
    EngineConfig, PageChannel, PageChannelReceiver, PageService, StorageEngine, disk_for,
};
use unbounded_storage::topology::DiskCpuSlot;

use crate::framework::executor::{Executor, RunError, yield_n, yield_once};
use crate::storage::mocks::{MockSimConfig, SimBlockDevice};

type EngineRc = Arc<StorageEngine<SimBlockDevice>>;
type CoreSlot = Rc<RefCell<CoreState>>;

enum CoreState {
    Pending(Option<PageChannelReceiver>),
    Ready(EngineRc, PageChannelReceiver),
    Taken,
}

#[derive(Clone, Debug)]
pub struct Workload {
    pub shard_count: usize,
    pub initial_disks: usize,
    pub device_pages: u64,
    pub max_io_delay: u32,
    pub key_count: u8,
    pub offset_count: u8,
    pub clients: Vec<ClientSpec>,
    pub applies: Vec<ApplySpec>,
}

#[derive(Clone, Debug)]
pub struct ClientSpec {
    pub ops: Vec<Op>,
}

#[derive(Clone, Debug)]
pub enum Op {
    Write {
        key_idx: u8,
        off_idx: u8,
        payload_seed: u8,
    },
    Read {
        key_idx: u8,
        off_idx: u8,
    },
}

#[derive(Clone, Debug)]
pub struct ApplySpec {
    pub delay: u32,
    pub kind: ApplyKind,
}

#[derive(Clone, Debug)]
pub enum ApplyKind {
    Noop,
    Peers { count: u8 },
    Backends { count: u8 },
    Frontends { count: u8 },
    DiskSwap { count: u8 },
}

#[derive(Debug)]
#[allow(dead_code)]
pub struct RunReport {
    pub steps: u64,
    pub shard_count: usize,
    pub client_count: usize,
    pub expected_ops: usize,
    pub completed_ops: usize,
    pub clients_finished: usize,
    pub channel_errors: usize,
    pub phase_a_ready: usize,
    pub phase_b_ready: usize,
    pub serve_before_phase_b: u64,
    pub shard_apply_counts: Vec<usize>,
    pub broadcasts: usize,
    pub disk_applies: usize,
    pub directory_generation: u64,
    pub max_snapshot_generation: u64,
    pub config_known: u64,
    pub config_applied: u64,
    pub registry_failures: usize,
}

#[derive(Debug)]
pub struct ExpectedApplyCounts {
    pub broadcasts: usize,
    pub disk_applies: usize,
}

impl Workload {
    fn page_size(&self) -> usize {
        4096
    }

    fn key(&self, idx: u8) -> StripeKey {
        StripeKey([idx % self.key_count.max(1); 32])
    }

    fn offset(&self, idx: u8) -> u64 {
        (idx as u64 % self.offset_count.max(1) as u64) * self.page_size() as u64
    }

    fn payload(&self, key_idx: u8, off_idx: u8, seed: u8) -> Vec<u8> {
        let mut out = vec![0u8; self.page_size()];
        let mix = key_idx.wrapping_mul(29) ^ off_idx.wrapping_mul(13) ^ seed;
        for (i, b) in out.iter_mut().enumerate() {
            *b = (i as u8).wrapping_add(mix);
        }
        out
    }
}

pub fn workload_strategy() -> impl Strategy<Value = Workload> {
    let applies = vec(apply_strategy(), 1..=5);
    (
        1usize..=4,
        1usize..=3,
        prop_oneof![1 => Just(0u32), 9 => 1u32..=4],
        1u8..=4,
        1u8..=4,
        vec(client_strategy(), 1..=4),
        applies,
        32u64..=128,
    )
        .prop_map(
            |(
                shard_count,
                initial_disks,
                max_io_delay,
                key_count,
                offset_count,
                clients,
                applies,
                device_pages,
            )| Workload {
                shard_count,
                initial_disks,
                device_pages,
                max_io_delay,
                key_count,
                offset_count,
                clients,
                applies,
            },
        )
}

fn client_strategy() -> impl Strategy<Value = ClientSpec> {
    vec(op_strategy(), 2..=8).prop_map(|ops| ClientSpec { ops })
}

fn op_strategy() -> impl Strategy<Value = Op> {
    prop_oneof![
        6 => (any::<u8>(), any::<u8>(), any::<u8>())
            .prop_map(|(k, o, s)| Op::Write { key_idx: k, off_idx: o, payload_seed: s }),
        4 => (any::<u8>(), any::<u8>())
            .prop_map(|(k, o)| Op::Read { key_idx: k, off_idx: o }),
    ]
}

fn apply_strategy() -> impl Strategy<Value = ApplySpec> {
    (
        0u32..=4,
        prop_oneof![
            1 => Just(ApplyKind::Noop),
            3 => (1u8..=4).prop_map(|count| ApplyKind::Peers { count }),
            2 => (1u8..=3).prop_map(|count| ApplyKind::Backends { count }),
            2 => (1u8..=3).prop_map(|count| ApplyKind::Frontends { count }),
            3 => (1u8..=4).prop_map(|count| ApplyKind::DiskSwap { count }),
        ],
    )
        .prop_map(|(delay, kind)| ApplySpec { delay, kind })
}

struct DiskGeneration {
    paths: Vec<PathBuf>,
    channels: Vec<PageChannel>,
    core_slots: Vec<CoreSlot>,
    stop: Rc<Cell<bool>>,
}

#[derive(Default)]
struct ApplyMetrics {
    broadcasts: usize,
    disk_applies: usize,
    registry_failures: usize,
    config_known: u64,
    config_applied: u64,
}

struct ShardState {
    phase_a: Cell<bool>,
    phase_b: Cell<bool>,
    stop: Cell<bool>,
    apply_queue: RefCell<VecDeque<u64>>,
    applied: RefCell<Vec<u64>>,
    serve_before_phase_b: Cell<u64>,
}

impl ShardState {
    fn new() -> Self {
        Self {
            phase_a: Cell::new(false),
            phase_b: Cell::new(false),
            stop: Cell::new(false),
            apply_queue: RefCell::new(VecDeque::new()),
            applied: RefCell::new(Vec::new()),
            serve_before_phase_b: Cell::new(0),
        }
    }
}

struct SimApplyTarget {
    shards: Vec<Rc<ShardState>>,
    directory: Arc<DiskChannelDirectory>,
    generations: Rc<Vec<DiskGeneration>>,
    next_generation: usize,
    current_generation: usize,
    registry: DiskRegistry<RegistryTarget>,
    metrics: Rc<RefCell<ApplyMetrics>>,
}

impl ConfigApplyTarget for SimApplyTarget {
    fn apply_in_place(&mut self, new: &Arc<Config>, diff: &ConfigDiff) -> Result<(), ApplyError> {
        let projection = runtime_projection(new)
            .map_err(|e| ApplyError::Target(format!("config projection failed: {e}")))?;

        if diff.requires_routing_reload()
            || diff.caches_changed
            || diff.backends_changed
            || diff.frontends_changed
        {
            self.metrics.borrow_mut().broadcasts += 1;
            for shard in &self.shards {
                shard.apply_queue.borrow_mut().push_back(new.version);
            }
        }

        if diff.caches_changed {
            let disks = runtime_disks(&projection);
            let report = self.registry.reconcile(&disks);
            let mut metrics = self.metrics.borrow_mut();
            metrics.disk_applies += 1;
            metrics.registry_failures += report.failures.len();
            drop(metrics);

            let prev = self.current_generation;
            let next = self.next_generation.min(self.generations.len() - 1);
            self.next_generation += 1;
            self.current_generation = next;
            publish_generation(&self.directory, &self.generations[next]);
            self.generations[prev].stop.set(true);
        }

        Ok(())
    }
}

#[derive(Clone, Copy)]
struct RegistryTarget;

impl DiskTarget for RegistryTarget {
    type Handle = usize;

    fn open(
        &self,
        _spec: &DiskSpec,
        _pin: Option<DiskCpuSlot>,
    ) -> Result<(Self::Handle, PageChannel), DiskError> {
        let (channel, _rx) = PageChannel::new();
        Ok((0, channel))
    }
}

pub fn run_workload(seed: u64, w: Workload) -> Result<RunReport, RunError> {
    let page_size = w.page_size();
    let expected_ops: usize = w.clients.iter().map(|c| c.ops.len()).sum();
    let pool_pages = expected_ops.max(1);
    let mut pool_buf: Box<[u8]> = vec![0u8; pool_pages * page_size].into_boxed_slice();
    let pool_base = pool_buf.as_mut_ptr() as usize;
    let pool_len = pool_pages * page_size;

    let directory = DiskChannelDirectory::new();
    let sim_cfg = MockSimConfig::new();
    sim_cfg.max_io_delay.set(w.max_io_delay);
    let generation_counts = generation_counts(&w);
    let generations = Rc::new(build_generations(&generation_counts));
    let devices = build_devices(&w, &generation_counts, &sim_cfg);

    let shard_states: Vec<Rc<ShardState>> = (0..w.shard_count)
        .map(|_| Rc::new(ShardState::new()))
        .collect();
    let peer_published = Rc::new(Cell::new(false));
    let serving_ready = Rc::new(Cell::new(false));
    let clients_finished = Rc::new(Cell::new(0usize));
    let completed_ops = Rc::new(Cell::new(0usize));
    let channel_errors = Rc::new(Cell::new(0usize));
    let max_snapshot_generation = Rc::new(Cell::new(0u64));
    let applies_done = Rc::new(Cell::new(false));
    let bootstrap_done = Rc::new(Cell::new(false));
    let metrics = Rc::new(RefCell::new(ApplyMetrics::default()));

    let mut exec = Executor::new(seed);

    spawn_bootstrap(
        &mut exec,
        w.clone(),
        generations.clone(),
        devices.clone(),
        directory.clone(),
        sim_cfg.clone(),
        pool_base,
        pool_len,
        bootstrap_done.clone(),
    );
    spawn_storage_cores(&mut exec, generations.clone());
    spawn_shards(&mut exec, &shard_states, peer_published.clone());
    spawn_phase_b_supervisor(
        &mut exec,
        &shard_states,
        peer_published.clone(),
        serving_ready.clone(),
    );
    spawn_apply_driver(
        &mut exec,
        w.clone(),
        shard_states.clone(),
        directory.clone(),
        generations.clone(),
        metrics.clone(),
        applies_done.clone(),
        bootstrap_done.clone(),
    );
    spawn_clients(
        &mut exec,
        w.clone(),
        directory.clone(),
        serving_ready.clone(),
        bootstrap_done.clone(),
        clients_finished.clone(),
        completed_ops.clone(),
        channel_errors.clone(),
        max_snapshot_generation.clone(),
        pool_base,
    );
    spawn_teardown_supervisor(
        &mut exec,
        shard_states.clone(),
        generations.clone(),
        clients_finished.clone(),
        applies_done.clone(),
        w.clients.len(),
    );

    let step_budget = 8192
        + expected_ops as u64 * (w.max_io_delay as u64 + 4) * 128
        + w.applies.len() as u64 * 4096
        + generations
            .iter()
            .map(|g| g.channels.len() as u64)
            .sum::<u64>()
            * 4096
        + w.shard_count as u64 * 2048;
    exec.run(step_budget)?;

    drop(pool_buf);
    let metrics = metrics.borrow();
    Ok(RunReport {
        steps: exec.last_steps(),
        shard_count: w.shard_count,
        client_count: w.clients.len(),
        expected_ops,
        completed_ops: completed_ops.get(),
        clients_finished: clients_finished.get(),
        channel_errors: channel_errors.get(),
        phase_a_ready: shard_states.iter().filter(|s| s.phase_a.get()).count(),
        phase_b_ready: shard_states.iter().filter(|s| s.phase_b.get()).count(),
        serve_before_phase_b: shard_states
            .iter()
            .map(|s| s.serve_before_phase_b.get())
            .sum(),
        shard_apply_counts: shard_states
            .iter()
            .map(|s| s.applied.borrow().len())
            .collect(),
        broadcasts: metrics.broadcasts,
        disk_applies: metrics.disk_applies,
        directory_generation: directory.generation(),
        max_snapshot_generation: max_snapshot_generation.get(),
        config_known: metrics.config_known,
        config_applied: metrics.config_applied,
        registry_failures: metrics.registry_failures,
    })
}

pub fn expected_apply_counts(w: &Workload) -> ExpectedApplyCounts {
    let mut current = config_for_generation(0, generation_disk_specs(0, w.initial_disks));
    let mut out = ExpectedApplyCounts {
        broadcasts: 0,
        disk_applies: 0,
    };

    for (idx, apply) in w.applies.iter().enumerate() {
        let mut next = current.clone();
        next.version = idx as u64 + 1;
        mutate_config(&mut next, apply, idx + 1);
        let diff = ConfigDiff::between(&current, &next);
        if diff.requires_routing_reload()
            || diff.caches_changed
            || diff.backends_changed
            || diff.frontends_changed
        {
            out.broadcasts += 1;
        }
        if diff.caches_changed {
            out.disk_applies += 1;
        }
        if diff.any() {
            current = next;
        }
    }

    out
}

fn generation_counts(w: &Workload) -> Vec<usize> {
    let mut counts = vec![w.initial_disks.max(1)];
    for apply in &w.applies {
        if let ApplyKind::DiskSwap { count } = apply.kind {
            counts.push(count.max(1) as usize);
        }
    }
    counts
}

fn build_generations(counts: &[usize]) -> Vec<DiskGeneration> {
    counts
        .iter()
        .enumerate()
        .map(|(generation, count)| {
            let mut paths = Vec::with_capacity(*count);
            let mut channels = Vec::with_capacity(*count);
            let mut core_slots = Vec::with_capacity(*count);
            for disk in 0..*count {
                paths.push(PathBuf::from(format!(
                    "/dst/generation-{generation}/disk-{disk}"
                )));
                let (channel, rx) = PageChannel::new();
                channels.push(channel);
                core_slots.push(Rc::new(RefCell::new(CoreState::Pending(Some(rx)))));
            }
            DiskGeneration {
                paths,
                channels,
                core_slots,
                stop: Rc::new(Cell::new(false)),
            }
        })
        .collect()
}

fn build_devices(
    w: &Workload,
    counts: &[usize],
    sim_cfg: &Rc<MockSimConfig>,
) -> Vec<Vec<Arc<SimBlockDevice>>> {
    counts
        .iter()
        .map(|count| {
            (0..*count)
                .map(|_| {
                    Arc::new(SimBlockDevice::new(
                        MockDeviceConfig {
                            page_size: w.page_size(),
                            capacity_pages: w.device_pages,
                            ..Default::default()
                        },
                        sim_cfg.clone(),
                    ))
                })
                .collect()
        })
        .collect()
}

fn spawn_bootstrap(
    exec: &mut Executor,
    w: Workload,
    generations: Rc<Vec<DiskGeneration>>,
    devices: Vec<Vec<Arc<SimBlockDevice>>>,
    directory: Arc<DiskChannelDirectory>,
    sim_cfg: Rc<MockSimConfig>,
    pool_base: usize,
    pool_len: usize,
    bootstrap_done: Rc<Cell<bool>>,
) {
    exec.spawn(async move {
        sim_cfg.max_io_delay.set(0);
        let engine_cfg = EngineConfig {
            page_size_bytes: w.page_size(),
            btree_page_bytes: w.page_size(),
            ..EngineConfig::default()
        };
        for (generation_idx, generation) in generations.iter().enumerate() {
            for (disk_idx, device) in devices[generation_idx].iter().enumerate() {
                let engine = Arc::new(
                    StorageEngine::open(device.clone(), engine_cfg.clone())
                        .await
                        .expect("lifecycle bootstrap opens storage engine"),
                );
                engine
                    .register_extra_buffer(pool_base as *mut u8, pool_len)
                    .expect("lifecycle bootstrap registers pool backing");
                let rx = {
                    let mut state = generation.core_slots[disk_idx].borrow_mut();
                    match &mut *state {
                        CoreState::Pending(rx) => rx.take().expect("receiver installed"),
                        CoreState::Ready(_, _) | CoreState::Taken => {
                            panic!("unexpected core bootstrap state")
                        }
                    }
                };
                *generation.core_slots[disk_idx].borrow_mut() = CoreState::Ready(engine, rx);
            }
        }
        sim_cfg.max_io_delay.set(w.max_io_delay);
        publish_generation(&directory, &generations[0]);
        bootstrap_done.set(true);
    });
}

fn spawn_storage_cores(exec: &mut Executor, generations: Rc<Vec<DiskGeneration>>) {
    for generation in generations.iter() {
        for slot in &generation.core_slots {
            let slot = slot.clone();
            let stop = generation.stop.clone();
            exec.spawn(async move {
                let (engine, rx) = loop {
                    let ready = {
                        let mut state = slot.borrow_mut();
                        match std::mem::replace(&mut *state, CoreState::Taken) {
                            CoreState::Ready(engine, rx) => Some((engine, rx)),
                            other => {
                                *state = other;
                                None
                            }
                        }
                    };
                    if let Some(pair) = ready {
                        break pair;
                    }
                    yield_once().await;
                };
                let mut service = PageService::new(engine.clone(), rx);
                let mut mutator: Pin<Box<dyn Future<Output = ()>>> =
                    Box::pin(engine.clone().run_mutator());
                let mut close_signaled = false;
                let mut mutator_done = false;
                loop {
                    poll_fn(|cx| {
                        if !mutator_done {
                            if let Poll::Ready(()) = mutator.as_mut().poll(cx) {
                                mutator_done = true;
                            }
                        }
                        service.poll_once(cx);
                        Poll::Ready(())
                    })
                    .await;
                    if (stop.get() || service.channel_disconnected()) && !close_signaled {
                        engine.close_mutator();
                        service.fail_all(Error::Io(libc::EIO));
                        service.mark_dead();
                        close_signaled = true;
                    }
                    if close_signaled {
                        service.drain_pending(Error::Io(libc::EIO));
                    }
                    if close_signaled && mutator_done && !service.has_inflight() {
                        return;
                    }
                    yield_once().await;
                }
            });
        }
    }
}

fn spawn_shards(
    exec: &mut Executor,
    shard_states: &[Rc<ShardState>],
    peer_published: Rc<Cell<bool>>,
) {
    for state in shard_states {
        let state = state.clone();
        let peer_published = peer_published.clone();
        exec.spawn(async move {
            let mut loop_driver = ShardLoop::new();
            let serving_state = state.clone();
            loop_driver.spawn(async move {
                while !serving_state.phase_b.get() && !serving_state.stop.get() {
                    yield_once().await;
                }
                while !serving_state.stop.get() {
                    if !serving_state.phase_b.get() {
                        serving_state
                            .serve_before_phase_b
                            .set(serving_state.serve_before_phase_b.get() + 1);
                    }
                    yield_once().await;
                }
            });
            let hook_state = state.clone();
            loop_driver.add_tick_hook(move || {
                let mut busy = false;
                if !hook_state.phase_a.get() {
                    hook_state.phase_a.set(true);
                    busy = true;
                }
                if peer_published.get() && !hook_state.phase_b.get() {
                    hook_state.phase_b.set(true);
                    busy = true;
                }
                if hook_state.phase_b.get() {
                    if let Some(version) = hook_state.apply_queue.borrow_mut().pop_front() {
                        hook_state.applied.borrow_mut().push(version);
                        busy = true;
                    }
                }
                busy
            });
            while !state.stop.get() || !loop_driver.is_empty() {
                loop_driver.tick();
                yield_once().await;
            }
        });
    }
}

fn spawn_phase_b_supervisor(
    exec: &mut Executor,
    shard_states: &[Rc<ShardState>],
    peer_published: Rc<Cell<bool>>,
    serving_ready: Rc<Cell<bool>>,
) {
    let shards = shard_states.to_vec();
    exec.spawn(async move {
        while !shards.iter().all(|s| s.phase_a.get()) {
            yield_once().await;
        }
        peer_published.set(true);
        while !shards.iter().all(|s| s.phase_b.get()) {
            yield_once().await;
        }
        serving_ready.set(true);
    });
}

fn spawn_apply_driver(
    exec: &mut Executor,
    w: Workload,
    shards: Vec<Rc<ShardState>>,
    directory: Arc<DiskChannelDirectory>,
    generations: Rc<Vec<DiskGeneration>>,
    metrics: Rc<RefCell<ApplyMetrics>>,
    applies_done: Rc<Cell<bool>>,
    bootstrap_done: Rc<Cell<bool>>,
) {
    exec.spawn(async move {
        while !bootstrap_done.get() || !shards.iter().all(|s| s.phase_b.get()) {
            yield_once().await;
        }
        let initial = Arc::new(config_for_generation(
            0,
            generation_disk_specs(0, w.initial_disks),
        ));
        let mut registry = DiskRegistry::new(RegistryTarget, disk_slots());
        let projection = runtime_projection(&initial).expect("initial sim config projects");
        let initial_disks = runtime_disks(&projection);
        let initial_report = registry.reconcile(&initial_disks);
        metrics.borrow_mut().registry_failures += initial_report.failures.len();
        let target = SimApplyTarget {
            shards,
            directory,
            generations,
            next_generation: 1,
            current_generation: 0,
            registry,
            metrics: metrics.clone(),
        };
        let mut controller = ConfigController::new(target, initial);
        for (idx, apply) in w.applies.iter().enumerate() {
            yield_n(apply.delay).await;
            let mut next = (*controller.current()).as_ref().clone();
            next.version = idx as u64 + 1;
            mutate_config(&mut next, apply, idx + 1);
            controller.apply(Arc::new(next)).expect("sim config apply");
        }
        let versions = controller.config_versions().snapshot();
        let mut m = metrics.borrow_mut();
        m.config_known = versions.known;
        m.config_applied = versions.applied;
        drop(m);
        applies_done.set(true);
    });
}

fn spawn_clients(
    exec: &mut Executor,
    w: Workload,
    directory: Arc<DiskChannelDirectory>,
    serving_ready: Rc<Cell<bool>>,
    bootstrap_done: Rc<Cell<bool>>,
    clients_finished: Rc<Cell<usize>>,
    completed_ops: Rc<Cell<usize>>,
    channel_errors: Rc<Cell<usize>>,
    max_snapshot_generation: Rc<Cell<u64>>,
    pool_base: usize,
) {
    let page_size = w.page_size();
    let mut base_slot = 0usize;
    for client in w.clients.clone() {
        let start_slot = base_slot;
        base_slot += client.ops.len();
        let w = w.clone();
        let directory = directory.clone();
        let serving_ready = serving_ready.clone();
        let bootstrap_done = bootstrap_done.clone();
        let clients_finished = clients_finished.clone();
        let completed_ops = completed_ops.clone();
        let channel_errors = channel_errors.clone();
        let max_snapshot_generation = max_snapshot_generation.clone();
        exec.spawn(async move {
            while !bootstrap_done.get() || !serving_ready.get() {
                yield_once().await;
            }
            for (op_idx, op) in client.ops.iter().enumerate() {
                let (snapshot, generation) = directory.snapshot();
                max_snapshot_generation.set(max_snapshot_generation.get().max(generation));
                let Some(channels) = snapshot.as_ref() else {
                    channel_errors.set(channel_errors.get() + 1);
                    completed_ops.set(completed_ops.get() + 1);
                    continue;
                };
                let slot = start_slot + op_idx;
                let base = unsafe { (pool_base as *mut u8).add(slot * page_size) };
                match op {
                    Op::Write {
                        key_idx,
                        off_idx,
                        payload_seed,
                    } => {
                        let key = w.key(*key_idx);
                        let offset = w.offset(*off_idx);
                        let payload = w.payload(*key_idx, *off_idx, *payload_seed);
                        unsafe {
                            std::ptr::write_bytes(base, 0, page_size);
                            std::ptr::copy_nonoverlapping(payload.as_ptr(), base, payload.len());
                        }
                        let src = std::ptr::slice_from_raw_parts(base as *const u8, payload.len());
                        let disk = disk_for(&key, offset, channels.len());
                        if channels[disk].write_page(key, offset, src).await.is_err() {
                            channel_errors.set(channel_errors.get() + 1);
                        }
                    }
                    Op::Read { key_idx, off_idx } => {
                        let key = w.key(*key_idx);
                        let offset = w.offset(*off_idx);
                        unsafe {
                            std::ptr::write_bytes(base, 0, page_size);
                        }
                        let dst = std::ptr::slice_from_raw_parts_mut(base, page_size);
                        let disk = disk_for(&key, offset, channels.len());
                        if channels[disk].read_page(key, offset, dst).await.is_err() {
                            channel_errors.set(channel_errors.get() + 1);
                        }
                    }
                }
                completed_ops.set(completed_ops.get() + 1);
            }
            clients_finished.set(clients_finished.get() + 1);
        });
    }
}

fn spawn_teardown_supervisor(
    exec: &mut Executor,
    shards: Vec<Rc<ShardState>>,
    generations: Rc<Vec<DiskGeneration>>,
    clients_finished: Rc<Cell<usize>>,
    applies_done: Rc<Cell<bool>>,
    client_count: usize,
) {
    exec.spawn(async move {
        loop {
            let queues_empty = shards.iter().all(|s| s.apply_queue.borrow().is_empty());
            if clients_finished.get() == client_count && applies_done.get() && queues_empty {
                for shard in &shards {
                    shard.stop.set(true);
                }
                for generation in generations.iter() {
                    generation.stop.set(true);
                }
                return;
            }
            yield_once().await;
        }
    });
}

fn publish_generation(directory: &DiskChannelDirectory, generation: &DiskGeneration) {
    let entries = generation
        .paths
        .iter()
        .cloned()
        .zip(generation.channels.iter().cloned())
        .enumerate()
        .map(|(idx, (path, channel))| (path, channel, Some((idx % 2) as u16)))
        .collect();
    directory.apply_channels(entries);
}

fn config_for_generation(version: u64, disks: Vec<DiskSpec>) -> Config {
    let mut cfg = Config::default();
    cfg.apply_defaults();
    cfg.version = version;
    cfg.backends = backend_specs(0, 1);
    cfg.self_ = "node-self".to_string();
    cfg.fingers_per_node = Some(100);
    cfg.peers = vec![self_peer_spec()];
    cfg.caches = vec![CacheSpec {
        name: "cache-0".to_string(),
        source: "backend-0".to_string(),
    }];
    cfg.disks = disks;
    cfg.frontends = frontend_specs(0, 1);
    cfg
}

fn mutate_config(cfg: &mut Config, apply: &ApplySpec, generation: usize) {
    match apply.kind {
        ApplyKind::Noop => {}
        ApplyKind::Peers { count } => {
            cfg.fingers_per_node = Some(count.max(1) as u32);
            cfg.peers = std::iter::once(self_peer_spec())
                .chain((0..count.max(1)).map(peer_spec_for))
                .collect();
        }
        ApplyKind::Backends { count } => {
            cfg.backends = backend_specs(generation, count.max(1));
        }
        ApplyKind::Frontends { count } => {
            cfg.frontends = frontend_specs(generation, count.max(1));
        }
        ApplyKind::DiskSwap { count } => {
            cfg.disks = generation_disk_specs(generation, count.max(1) as usize);
        }
    }
}

fn backend_specs(generation: usize, count: u8) -> Vec<BackendSpec> {
    (0..count.max(1))
        .map(|i| BackendSpec {
            name: if i == 0 {
                "backend-0".to_string()
            } else {
                format!("backend-{generation}-{i}")
            },
            config: Some(backend_spec::Config::Http(HttpBackendConfig {
                url: format!("https://origin-{generation}-{i}.example.com"),
                stripe_size_bytes: Some(4 * 1024 * 1024),
                http_concurrency: Some(64),
                ca_cert_path: None,
                insecure_skip_verify: false,
            })),
        })
        .collect()
}

fn frontend_specs(generation: usize, count: u8) -> Vec<FrontendSpec> {
    (0..count.max(1))
        .map(|i| FrontendSpec {
            name: format!("frontend-{generation}-{i}"),
            source: "cache-0".to_string(),
            config: Some(frontend_spec::Config::Http(HttpFrontendConfig {
                addr: format!("127.0.0.1:{}", 10_000 + generation as u16 * 16 + i as u16),
                max_requests_per_connection: None,
            })),
        })
        .collect()
}

fn peer_spec_for(idx: u8) -> PeerSpec {
    PeerSpec {
        name: format!("node-{idx}"),
        tags: vec![format!("rack-{}", idx % 2)],
        config: Some(peer_spec::Config::Tcp(TcpPeerConfig {
            addr: format!("127.0.0.1:{}", 9000 + idx as u16),
        })),
    }
}

fn self_peer_spec() -> PeerSpec {
    PeerSpec {
        name: "node-self".to_string(),
        tags: vec!["rack-self".to_string()],
        config: Some(peer_spec::Config::Tcp(TcpPeerConfig {
            addr: "127.0.0.1:8999".to_string(),
        })),
    }
}

fn generation_disk_specs(generation: usize, count: usize) -> Vec<DiskSpec> {
    (0..count.max(1))
        .map(|idx| DiskSpec {
            queue_depth: Some(32),
            page_size_bytes: Some(4096),
            skip_recovery_scan: true,
            config: Some(disk_spec::Config::File(FileDiskConfig {
                size: Some(64 * 1024 * 1024),
                path: format!("/dst/generation-{generation}/disk-{idx}"),
            })),
        })
        .collect()
}

fn disk_slots() -> Vec<DiskCpuSlot> {
    (0..4)
        .map(|idx| DiskCpuSlot {
            cpu: idx as u32,
            numa: Some((idx % 2) as u16),
        })
        .collect()
}
