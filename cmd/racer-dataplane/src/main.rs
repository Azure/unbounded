// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Dynamically configured cache (Linux 6.1+, NUMA binding and memlock required).
//!
//! RACER_CONTROL_PLANE_URL: HTTPS subscription URL or watched ProtoJSON file.
//! RACER_SLAB_PATH: default cache.slab; runtime resize atomically replaces its inode.
//! RACER_SLAB_SIZE: size in bytes for a NEW slab, default 10 GiB.
//! RACER_SHARDS: optional initial shard count and execution worker cap.
//! RACER_IO_WORKERS / RACER_COMPUTE_WORKERS: optional positive counts PER NUMA node.
//! Default: split allowed physical cores evenly (odd core goes to I/O). One
//! override gives the other pool the remaining cores; two may leave cores idle.
//! Execution planning retains a default eight-worker cap; explicit RACER_SHARDS
//! also sets this cap. Automatic storage shards scale independently with size.
//! Persisted slabs require the same actual total I/O worker count; automatic
//! startup discovers their recorded shard count.
//! Legacy slabs without placement metadata require an explicit fresh cache path.
//! Compute threads calculate and validate CRC64 before publishing incoming values.
//! RACER_BUFFERS_PER_NODE: transient 64 MiB buffer count, minimum 4, default 8.
//! RACER_METRICS_ADDR: management listener override (numeric socket address).
//! RACER_POD_IP: default management bind IP on port 9090; unset uses 0.0.0.0.
//! RACER_STARTUP_SECONDS / RACER_STALL_SECONDS: startup/progress limits, 90 / 5.
//! RACER_DRAIN_SECONDS / RACER_QUIESCE_SECONDS: graceful/hard exit budgets, 20 / 5.
//! RACER_RDMA_MODE: disabled (default) or enabled with RACER_RDMA_RAILS selectors.
//! RACER_RDMA_CONNECTIONS / RACER_RDMA_DEPTH: per worker/rail, defaults 8 / 2.
//! RDMA catalog and execution placement changes require restart.
//!
//! RACER_UNIVERSE / RACER_NODE: required 32-byte hexadecimal bootstrap identities.
//! RACER_TLS_TRUST_DIR: projected CA bundle directory, default /var/run/racer-trust.
//! RACER_ENROLL_URL / RACER_CONTROL_SERVER_NAME: HTTPS enrollment and CP DNS identity.
//! RACER_CONTROL_TOKEN_FILE: enrollment token with audience racer-control.
//! RACER_POD_NAMESPACE / RACER_POD_NAME / RACER_POD_UID: enrollment Pod identity.
//! Remote control requires node mTLS. Invalid updates retain the last configuration.

mod version;

use racer_dataplane::{
    allocator::{self, Slab},
    buffers,
    cache::{Cache, Namespace},
    control, crypto, lifecycle, metrics, rdma, runtime, uring, workers,
};
use std::{
    env, io,
    net::{IpAddr, Ipv4Addr, SocketAddr},
    num::NonZeroUsize,
    str::FromStr,
    sync::{
        Arc, Mutex,
        atomic::{AtomicBool, Ordering},
    },
};

const BUDGET: usize = 128;
#[cfg(test)]
use std::{thread, time::Duration};
static STOP: AtomicBool = AtomicBool::new(false);

fn setting<T: FromStr>(name: &str, default: &str) -> io::Result<T> {
    let value = match env::var(name) {
        Ok(value) => value,
        Err(env::VarError::NotPresent) => default.to_owned(),
        Err(error) => return Err(io::Error::new(io::ErrorKind::InvalidInput, error)),
    };
    value.parse().map_err(|_| {
        io::Error::new(
            io::ErrorKind::InvalidInput,
            format!("invalid {name}: {value}"),
        )
    })
}

fn optional_count(name: &str) -> io::Result<Option<NonZeroUsize>> {
    match env::var(name) {
        Ok(value) => value.parse().map(Some).map_err(|_| {
            io::Error::new(
                io::ErrorKind::InvalidInput,
                format!("invalid {name}: {value}"),
            )
        }),
        Err(env::VarError::NotPresent) => Ok(None),
        Err(error) => Err(io::Error::new(io::ErrorKind::InvalidInput, error)),
    }
}

fn daemon_pool_config(count: NonZeroUsize) -> io::Result<buffers::Config> {
    // Configuration can later activate any supported topology: three peer hops
    // reserve three slots, and the receiving producer needs one more. Reject
    // undersized daemon pools even if the initial topology happens to be local.
    if count.get() < 4 {
        return Err(io::Error::new(
            io::ErrorKind::InvalidInput,
            "RACER_BUFFERS_PER_NODE must be at least 4: three-hop peer receive reservation requires three free downstream slots plus one receive slot",
        ));
    }
    Ok(buffers::Config::new(count))
}

// Select the primary Pod IP before any topology is downloaded: kubelet probes
// must reach management even while data listeners are not yet ready. Keep the
// explicit socket override authoritative, including its address family and port.
fn management_address(
    mut lookup: impl FnMut(&str) -> Result<String, env::VarError>,
) -> io::Result<SocketAddr> {
    match lookup("RACER_METRICS_ADDR") {
        Ok(value) => {
            return value.parse().map_err(|_| {
                io::Error::new(
                    io::ErrorKind::InvalidInput,
                    format!("invalid RACER_METRICS_ADDR: {value}"),
                )
            });
        }
        Err(env::VarError::NotPresent) => {}
        Err(error) => return Err(io::Error::new(io::ErrorKind::InvalidInput, error)),
    }
    Ok(SocketAddr::new(pod_ip(lookup)?, 9090))
}

// Peer reachability follows the advertised primary Pod IP, independently of
// management overrides. The network peer protocol always uses TLS port 9443.
fn peer_address(
    lookup: impl FnMut(&str) -> Result<String, env::VarError>,
) -> io::Result<SocketAddr> {
    Ok(SocketAddr::new(pod_ip(lookup)?, 9443))
}

fn pod_ip(mut lookup: impl FnMut(&str) -> Result<String, env::VarError>) -> io::Result<IpAddr> {
    match lookup("RACER_POD_IP") {
        Ok(value) => value.parse::<IpAddr>().map_err(|_| {
            io::Error::new(
                io::ErrorKind::InvalidInput,
                format!("invalid RACER_POD_IP: {value}"),
            )
        }),
        Err(env::VarError::NotPresent) => Ok(Ipv4Addr::UNSPECIFIED.into()),
        Err(error) => Err(io::Error::new(io::ErrorKind::InvalidInput, error)),
    }
}

struct Application(runtime::Volumes);
impl uring::Application for Application {
    fn begin_drain(&mut self) {
        self.0.begin_drain();
    }
    fn drained(&self) -> bool {
        self.0.drained()
    }
    fn poll(&mut self, ring: &mut uring::Ring, budget: usize) -> io::Result<uring::Work> {
        self.0.poll(ring, budget)
    }
    fn shutdown(&mut self, ring: &mut uring::Ring) -> io::Result<()> {
        self.0.shutdown(ring)
    }
}

extern "C" fn stop_signal(_: libc::c_int) {
    STOP.store(true, Ordering::Relaxed);
}
fn install_signals() -> io::Result<()> {
    // SAFETY: initialized sigaction; handler only stores a lock-free atomic.
    unsafe {
        let mut action: libc::sigaction = std::mem::zeroed();
        action.sa_sigaction = stop_signal as *const () as usize;
        action.sa_flags = libc::SA_RESTART;
        libc::sigemptyset(&mut action.sa_mask);
        for signal in [libc::SIGINT, libc::SIGTERM] {
            if libc::sigaction(signal, &action, std::ptr::null_mut()) != 0 {
                return Err(io::Error::last_os_error());
            }
        }
    }
    Ok(())
}

fn main() -> io::Result<()> {
    main_with_args(env::args_os().skip(1))
}

fn main_with_args(args: impl Iterator<Item = std::ffi::OsString>) -> io::Result<()> {
    if version::print_requested(args)? {
        return Ok(());
    }
    install_signals()?;
    let life = Arc::new(lifecycle::Lifecycle::new(lifecycle::Config::from_env()?));
    let stop = workers::StopHandle::supervised(life.clone());
    // Declared first: remains armed through every later destructor/join, including
    // failed startup and a blocked provider or Subscriber teardown.
    let _monitor = lifecycle::Monitor::start(life.clone(), stop.clone(), &STOP)?;
    run(life, stop)
}

fn run(life: Arc<lifecycle::Lifecycle>, stop: workers::StopHandle) -> io::Result<()> {
    stop.check_startup()?;
    let io_stop = stop.clone();
    let slab_io = racer_dataplane::slab_io::Io::new(racer_dataplane::slab_io::Config::from_env()?)
        .with_stop(move || io_stop.is_stopping());
    let mut pool_config = daemon_pool_config(setting("RACER_BUFFERS_PER_NODE", "8")?)?;
    pool_config.consumers_per_flight = setting("RACER_FLIGHT_CONSUMERS", "64")?;
    let source = control::Source::from_env()?;
    let peer = peer_address(|name| env::var(name))?;
    let trust = Arc::new(control::Trust::from_env()?);
    stop.check_startup()?;
    let updates = Arc::new(control::Updates::default());
    updates.set_lifecycle(life.clone());
    let config = workers::Config {
        shard_count: setting("RACER_SHARDS", "8")?,
    };
    let plan = workers::CpuPlan::discover(
        config,
        workers::WorkerCounts {
            io_per_node: optional_count("RACER_IO_WORKERS")?,
            compute_per_node: optional_count("RACER_COMPUTE_WORKERS")?,
        },
    )?;
    life.configure_workers(plan.io().len());
    let _credentials = if matches!(source, control::Source::Http { .. }) {
        Some(loop {
            stop.check_startup()?;
            match control::credentials::Manager::start(plan.io().len(), &updates) {
                Ok(manager) => break manager,
                Err(error) => eprintln!("TLS enrollment unavailable: {error}"),
            }
            // Reload projected trust on every attempt, including initial startup.
            for _ in 0..10 {
                stop.check_startup()?;
                std::thread::sleep(std::time::Duration::from_millis(100));
            }
        })
    } else {
        None
    };
    let rdma_policy = rdma::StartupPolicy::from_env()?;
    let windows = rdma_policy.validate_workers(plan.io().len())?;
    eprintln!(
        "RDMA process provisioning ceiling: {windows} windows, {} control bytes; payload pool registration additional per worker/rail",
        rdma_policy.control_bytes(plan.io().len())?
    );
    let rails = rdma_policy.catalog();
    let path: String = setting("RACER_SLAB_PATH", "cache.slab")?;
    let storage_path = runtime::StoragePath::lock(&path)?;
    // Validate persisted placement before any listener or worker is started.
    let budget = allocator::CheckpointBudget::default();
    let mut slab = slab_io.scope(|| {
        match Slab::open_existing_layout(storage_path.active(), plan.io().len()) {
            Ok(slab) => Ok(slab),
            Err(error) if error.kind() == io::ErrorKind::NotFound => {
                let size = setting("RACER_SLAB_SIZE", &allocator::DEFAULT_SLAB_SIZE.to_string())?;
                if env::var_os("RACER_SHARDS").is_some() {
                    Slab::open_or_create_layout(
                        storage_path.active(),
                        size,
                        config.shard_count.get(),
                        plan.io().len(),
                    )
                } else {
                    allocator::LayoutPlan::new(size, plan.io().len())?
                        .create(storage_path.active(), budget.clone())
                }
            }
            Err(error) => Err(error),
        }
    })?;
    slab.set_checkpoint_budget(budget.clone())?;
    runtime::validate_startup_memory(&slab, plan.io().len())?;
    let storage_generation = plan.io()[0].storage_generation(slab.shard_count())?;
    let storage = runtime::StorageCoordinator::start(
        storage_path,
        &slab,
        &plan.io()[0],
        budget,
        updates.clone(),
    )?;
    let storage_handle = storage.handle();
    stop.check_startup()?;
    let crypto_count = plan.compute().cpus().len();
    let registry = Arc::new(
        metrics::Registry::new(plan.io().len(), updates.clone())
            .with_lifecycle(life.clone())
            .with_slab_io(slab_io),
    );
    let exporter =
        metrics::Exporter::start(management_address(|name| env::var(name))?, registry.clone())?;
    eprintln!("Prometheus metrics listening on {}", exporter.address());
    let cache_limits = racer_dataplane::cache::Limits {
        active_faults: setting("RACER_ACTIVE_FAULTS", "128")?,
        internal_reserve: setting("RACER_INTERNAL_FAULT_RESERVE", "32")?,
        resource_retries: setting("RACER_RESOURCE_RETRIES", "32")?,
    };
    // Setup-only lock, never acquired in a worker's I/O loop.
    let slab = Mutex::new((Some(slab), plan.io().len()));
    stop.check_startup()?;
    let pools = buffers::Pools::new(pool_config);
    let crypto = Arc::new(crypto::Pool::start(
        plan.compute(),
        crypto::PoolConfig::default(),
    )?);
    let worker_crypto = crypto.clone();
    let control_updates = updates.clone();
    let factory_stop = stop.clone();
    let workers = workers::Workers::start_supervised(plan, stop, move |placement| {
        let pool = pools.for_worker(placement)?;
        factory_stop.check_startup()?;
        let ring = uring::Ring::new(placement, pool.clone(), uring::Config::default())?;
        registry.register(placement.worker_id().0, ring.metrics());
        let capabilities = {
            let mut setup = slab.lock().unwrap();
            let capabilities = storage_generation
                .take_assignments(placement)?
                .into_iter()
                .map(|assignment| {
                    setup
                        .0
                        .as_mut()
                        .unwrap()
                        .take_shard(assignment.id())
                        .map(|slab| (assignment, slab))
                })
                .collect::<io::Result<Vec<_>>>()?;
            setup.1 -= 1;
            if setup.1 == 0 {
                setup.0.take();
            }
            capabilities
        };
        let caches = capabilities
            .into_iter()
            .map(|(assignment, slab)| {
                racer_dataplane::sharding::ShardState::activate(
                    placement,
                    assignment,
                    slab,
                    &pool,
                    allocator::Config::default(),
                )
            })
            .collect::<io::Result<Vec<_>>>()?;
        let mut cache = Cache::for_generation(
            placement,
            &storage_generation,
            Namespace::new("bootstrap").map_err(io::Error::other)?,
            caches,
        )
        .map_err(io::Error::other)?;
        cache.set_limits(cache_limits).map_err(io::Error::other)?;
        cache.set_metrics(ring.metrics().clone());
        updates.subscribe(ring.wake_handle());
        let app = runtime::Volumes::new(
            cache,
            updates.clone(),
            worker_crypto.clone(),
            placement.worker_id().0,
        )
        .with_storage(storage_handle.clone(), placement.clone())
        .with_peer_ip(peer.ip())
        .with_rdma_startup(rdma_policy.clone(), rails.clone());
        Ok(uring::Driver::new(ring, Application(app), BUDGET)?
            .with_lifecycle(life.clone(), placement.worker_id().0))
    })?;
    let _subscriber = control::Subscriber::start(source, trust, control_updates)?;
    eprintln!(
        "control-plane subscription started with {} I/O worker(s), {crypto_count} compute worker(s)",
        workers.placements().len()
    );
    let result = workers.join();
    let crypto_result = Arc::try_unwrap(crypto)
        .map_err(|_| io::Error::other("crypto pool still attached"))?
        .shutdown();
    result?;
    crypto_result.map_err(io::Error::other)
}

#[cfg(test)]
#[path = "../tests/bin/version.rs"]
mod version_tests;

#[cfg(test)]
include!(concat!(
    env!("CARGO_MANIFEST_DIR"),
    "/tests/bin/dataplane.rs"
));
