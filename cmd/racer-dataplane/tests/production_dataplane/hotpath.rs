//! Opt-in bottleneck probe, reusing the production graph fixture above.
//!
//! Automatic sizing has no historical four-shard cap. Count distinct crypto groups,
//! and scale RAM-disk capacity and memory preflight with the selected I/O shards.
//! Requests use v2 POST subscriptions with Page, Complete, and EOF validation and
//! reconnect per request. Focused harness checks are not new performance results;
//! historical paired-worker measurements do not characterize this shared policy.
use super::*;
use racer_dataplane::config::Config;
use racer_dataplane::config::DEFAULT_MAX_THREADS;
use racer_dataplane::worker::AffinityPlan;
use racer_dataplane::worker::Resources;
use racer_dataplane::worker::WorkerGroup;
use racer_dataplane::worker::WorkerRuntime;
use std::collections::VecDeque;
use std::io::BufReader;
use std::sync::Barrier;
use std::sync::mpsc;

const REQUESTS: usize = 128;
const SWEEPS: usize = 4;
const MIB: u64 = 1 << 20;

fn default_config() -> Config {
    // Parse real defaults without inheriting ambient RACER_* overrides or loading files.
    let config = fixture_io::default_config(CLUSTER);
    assert_eq!(config.max_threads, DEFAULT_MAX_THREADS);
    config
}

fn default_plan() -> AffinityPlan {
    let config = default_config();
    let topology = EffectiveTopology::discover().unwrap();
    println!(
        "allowed_cpus={:?} cpu_quota={:?} default_max_threads={DEFAULT_MAX_THREADS}",
        topology.cpus, topology.quota
    );
    let plan = AffinityPlan::from_topology(&config, topology, &[]).unwrap();
    // This component fixture supplies per-shard budgets, not production's
    // aggregate budget partitioning and worker reduction.
    let n = plan.pairs.len();
    assert!(n > 0);
    println!(
        "host_default_io_shards={n} crypto_threads={} layout={:?}",
        plan.crypto_groups().len(),
        plan.pairs
    );
    plan
}

fn owned_plan(plan: &AffinityPlan) -> AffinityPlan {
    AffinityPlan {
        pairs: plan.pairs.clone(),
        // Automatic mode already has room for the driver; finite caps need one
        // extra slot without changing any I/O-to-crypto assignments.
        max_threads: plan.max_threads.saturating_add(1),
    }
}

// A full encrypted page plus its header allows three records per 64 MiB segment.
// Include one warm page per owner and one free segment. The RAM disk grows with
// aggregate sparse lengths and retains filesystem headroom.
fn slab_bytes(workers: usize) -> u64 {
    ((REQUESTS.div_ceil(workers) + 1).div_ceil(3) + 1) as u64 * 64 * MIB
}

fn object(key: usize) -> ObjectId {
    ObjectId {
        cache: CacheId(CACHE.into()),
        key: CacheKey::parse_hex(format!("{key:064x}").as_bytes()).unwrap(),
    }
}

// Pick distinct keys with round-robin page-zero/metadata owners using the actual
// production hash. Selection is bounded and takes place outside timed work.
fn balanced_keys(map: &WorkerMap, workers: usize, count: usize) -> Vec<usize> {
    let mut buckets = vec![VecDeque::new(); workers];
    let per_worker = count.div_ceil(workers);
    for key in 0..1_000_000 {
        let owner = map.metadata_owner(&object(key)).unwrap().0 as usize;
        if buckets[owner].len() < per_worker {
            buckets[owner].push_back(key);
        }
        if buckets.iter().all(|keys| keys.len() == per_worker) {
            return (0..count)
                .map(|index| buckets[index % workers].pop_front().unwrap())
                .collect();
        }
    }
    panic!("could not find balanced fixture keys");
}

#[derive(Debug)]
struct Stats {
    records: usize,
    discarded: u64,
    origin_calls: usize,
    failures: BTreeMap<String, usize>,
    requests: usize,
}

enum Command {
    Connect(UnixStream),
    Inspect {
        evict: bool,
        reply: mpsc::Sender<Stats>,
    },
    Offline,
}
struct Factory {
    resources: Resources,
    root: PathBuf,
    commands: Mutex<BTreeMap<u16, mpsc::Receiver<Command>>>,
    directory: Arc<WorkerDirectory>,
    slab_bytes: u64,
}
fn budgets() -> Limits {
    let mut limits = limits(16);
    limits.ciphertext_bytes = nz(512 << 20);
    limits.dirty_bytes = nz(128 << 20);
    limits.client_connections = nz(256);
    limits.request_context_bytes = nz(16 << 20);
    limits.queue_entries = nz(256);
    limits.pipes = nz(16);
    limits
}
impl Factory {
    fn build(
        &self,
        worker: WorkerId,
        runtime: WorkerRuntime,
    ) -> Result<Box<dyn uring_runtime::group::Service<RequestScope>>> {
        let rig = Rc::new(Rig::assemble(
            P,
            false,
            16,
            8,
            Some(RigWorker {
                scratch: Scratch::under(&self.root),
                runtime,
                worker,
                directory: self.directory.clone(),
                slab_bytes: self.slab_bytes,
            }),
        ));
        Ok(Box::new(Service {
            rig,
            commands: self.commands.lock().unwrap().remove(&worker.0).unwrap(),
            clients: VecDeque::new(),
            inspection: None,
            failures: Rc::new(RefCell::new(BTreeMap::new())),
            requests: Rc::new(std::cell::Cell::new(0)),
        }))
    }
    fn build_crypto(
        &self,
        _: WorkerId,
        runtime: CryptoRuntime,
    ) -> Result<Box<dyn uring_runtime::group::Service<RequestScope>>> {
        Ok(Box::new(PageCryptoEngine::new(runtime)))
    }
}
impl uring_runtime::group::Factory<RequestScope> for Factory {
    fn build_lane(
        &self,
        lane: usize,
    ) -> Result<Box<dyn uring_runtime::group::Service<RequestScope>>> {
        self.resources
            .build_lane(lane, |worker, runtime| self.build(worker, runtime))
    }
    fn build_helper(
        &self,
        lane: usize,
    ) -> Result<Box<dyn uring_runtime::group::Service<RequestScope>>> {
        self.resources
            .build_helper(lane, |worker, runtime| self.build_crypto(worker, runtime))
    }
    fn abandon_lane(&self, lane: usize) -> Result<()> {
        self.resources.abandon_lane(lane)
    }
    fn teardown_scope(&self, _: &RequestScope) -> RequestScope {
        scope()
    }
}
struct Service {
    rig: Rc<Rig>,
    commands: mpsc::Receiver<Command>,
    clients: VecDeque<Operation<'static, ()>>,
    inspection: Option<(bool, mpsc::Sender<Stats>)>,
    failures: Rc<RefCell<BTreeMap<String, usize>>>,
    requests: Rc<std::cell::Cell<usize>>,
}
impl Service {
    fn poll(&mut self, cx: &mut Context<'_>) -> Result<()> {
        let _queue = self.rig.drivers.enter();
        // WorkerGroup polls the reactor and crypto completions with its real waker.
        self.rig.endpoint.borrow_mut().poll(cx, 64)?;
        self.rig.flights.poll_with_context(cx, 64)?;
        for command in self.commands.try_iter().take(256) {
            match command {
                Command::Connect(socket) => {
                    let rig = self.rig.clone();
                    let failures = self.failures.clone();
                    let requests = self.requests.clone();
                    self.clients.push_back(Box::pin(async move {
                        let lease =
                            racer_dataplane::http::from_accepted(socket.into(), &rig.admission)?;
                        let scope = scope();
                        let received = rig.io.receive_head(lease, &scope).await?;
                        let request = RequestParser::new(32768)
                            .parse(&CacheId(CACHE.into()), received.value)?;
                        let kind = request.kind.clone();
                        requests.set(requests.get() + 1);
                        match rig.coordinator.read(request, &scope).await {
                            Ok(response) => {
                                rig.responses.validate(&kind, &response)?;
                                let sent = rig
                                    .responses
                                    .send_subscription_unobserved(
                                        received.connection,
                                        response,
                                        &scope,
                                        TIMEOUT,
                                    )
                                    .await;
                                if let Err(error) = &sent {
                                    *failures
                                        .borrow_mut()
                                        .entry(format!("body:{error:?}"))
                                        .or_default() += 1;
                                }
                                // Every v2 subscription closes after completion.
                                drop(sent?);
                                Ok(())
                            }
                            Err(error) => {
                                *failures
                                    .borrow_mut()
                                    .entry(format!("read:{error:?}"))
                                    .or_default() += 1;
                                rig.responses
                                    .send_error(received.connection, error, &scope)
                                    .await?;
                                Ok(())
                            }
                        }
                    }));
                }
                Command::Inspect { evict, reply } => {
                    assert!(self.inspection.is_none());
                    self.inspection = Some((evict, reply));
                }
                Command::Offline => self.rig.adapter.offline(),
            }
        }
        for _ in 0..self.clients.len().min(256) {
            let mut task = self.clients.pop_front().unwrap();
            if task.as_mut().poll(cx).is_pending() {
                self.clients.push_back(task);
            }
        }
        let mut task = self.rig.writer_task.borrow_mut();
        if let Some(write) = task.as_mut()
            && let Poll::Ready(result) = write.as_mut().poll(cx)
        {
            result?;
            *task = None;
        }
        if task.is_none() && self.rig.writer.pending_count() > 0 {
            let writer = self.rig.writer.clone();
            *task = Some(Box::pin(async move {
                writer.progress(1, &scope()).await.map(|_| ())
            }));
            cx.waker().wake_by_ref();
        }
        if task.is_none()
            && self.rig.writer.is_idle()
            && let Some((evict, reply)) = self.inspection.take()
        {
            if evict {
                self.rig.memory.evict_idle(usize::MAX)?;
                assert_eq!(self.rig.admission.used(ResourceClass::Plaintext), 0);
            }
            let entries = self.rig.writer.index().snapshot()?.entries.len();
            let _ = reply.send(Stats {
                records: entries,
                discarded: self.rig.writer.discarded_count(),
                origin_calls: self.rig.adapter.calls().len(),
                failures: self.failures.borrow().clone(),
                requests: self.requests.get(),
            });
        }
        Ok(())
    }
}
impl uring_runtime::group::Service<RequestScope> for Service {
    fn start<'a>(&'a mut self, _: &'a RequestScope) -> Operation<'a, ()> {
        Box::pin(async { Ok(()) })
    }
    fn poll_budgeted(&mut self, cx: &mut Context<'_>, _: usize) -> Result<()> {
        self.poll(cx)
    }
    fn stop_admission(&mut self) -> Result<()> {
        Ok(())
    }
    fn drain<'a>(&'a mut self, scope: &'a RequestScope) -> Operation<'a, ()> {
        Box::pin(std::future::poll_fn(move |cx| {
            scope.check()?;
            self.poll(cx)?;
            if self.clients.is_empty()
                && self.rig.writer.is_idle()
                && self.rig.writer_task.borrow().is_none()
            {
                Poll::Ready(Ok(()))
            } else {
                Poll::Pending
            }
        }))
    }
    fn shutdown<'a>(&'a mut self, scope: &'a RequestScope) -> Operation<'a, ()> {
        self.drain(scope)
    }
}

fn inspect(commands: &mpsc::SyncSender<Command>, evict: bool) -> Stats {
    let (reply, result) = mpsc::channel();
    commands.send(Command::Inspect { evict, reply }).unwrap();
    result.recv_timeout(TIMEOUT).unwrap()
}
fn inspect_all(commands: &[mpsc::SyncSender<Command>], evict: bool) -> Vec<Stats> {
    commands
        .iter()
        .map(|commands| inspect(commands, evict))
        .collect()
}

fn connect(commands: &mpsc::SyncSender<Command>) -> BufReader<UnixStream> {
    let (server, client) = UnixStream::pair().unwrap();
    client.set_read_timeout(Some(TIMEOUT)).unwrap();
    client.set_write_timeout(Some(TIMEOUT)).unwrap();
    commands.send(Command::Connect(server)).unwrap();
    BufReader::with_capacity(64 << 10, client)
}
fn get(client: &mut BufReader<UnixStream>, key: usize, verify: bool) -> std::io::Result<u16> {
    use std::io::BufRead;
    write!(
        client.get_mut(),
        "POST /v2/objects/{key:064x} HTTP/1.1\r\nHost: racer\r\nContent-Length: 0\r\nRacer-Page-Credits: 1\r\nRacer-Byte-Credits: 16777216\r\nRacer-Ordered: 1\r\nAuthorization: fixture-credential\r\nRacer-Metadata: fixture-metadata\r\nIf-Match: \"v1\"\r\nRange: bytes=0-16777215\r\nConnection: close\r\n\r\n"
    )?;
    let mut head = String::new();
    loop {
        let start = head.len();
        if client.read_line(&mut head)? == 0 {
            return Err(std::io::Error::new(
                std::io::ErrorKind::UnexpectedEof,
                "truncated response head",
            ));
        }
        assert!(head.len() <= 32768);
        if &head[start..] == "\r\n" {
            break;
        }
    }
    let status: u16 = head.split_whitespace().nth(1).unwrap().parse().unwrap();
    let fields = fields(&head);
    let wire_length: usize = fields["content-length"].parse().unwrap();
    let length = if status == 200 {
        assert_eq!(wire_length, P as usize + 42);
        assert_eq!(fields["etag"], "\"v1\"");
        assert_eq!(fields["racer-object-length"], P.to_string());
        assert_eq!(fields["racer-range-start"], "0");
        assert_eq!(fields["racer-range-end"], P.to_string());
        assert_eq!(fields["connection"], "close");
        let mut frame = [0; 21];
        client.read_exact(&mut frame)?;
        assert_eq!(frame[0], 1);
        assert_eq!(&frame[1..17], &[0; 16]);
        assert_eq!(
            u32::from_be_bytes(frame[17..].try_into().unwrap()),
            P as u32
        );
        P as usize
    } else {
        assert_eq!(wire_length, 0);
        wire_length
    };
    let mut scratch = [0; 64 << 10];
    let mut offset = 0;
    while offset < length {
        let n = scratch.len().min(length - offset);
        client.read_exact(&mut scratch[..n])?;
        if status == 200 {
            if verify {
                for (i, value) in scratch[..n].iter().enumerate() {
                    assert_eq!(*value, byte(1, (offset + i) as u64));
                }
            } else {
                assert_eq!(scratch[0], byte(1, offset as u64));
                assert_eq!(scratch[n - 1], byte(1, (offset + n - 1) as u64));
            }
        }
        offset += n;
    }
    if status == 200 {
        let mut complete = [0; 21];
        client.read_exact(&mut complete)?;
        assert_eq!(complete[0], 2);
        assert_eq!(u64::from_be_bytes(complete[1..9].try_into().unwrap()), 1);
        assert_eq!(u64::from_be_bytes(complete[9..17].try_into().unwrap()), P);
        assert_eq!(&complete[17..], &[0; 4]);
        assert_eq!(client.read(&mut scratch[..1])?, 0);
    }
    Ok(status)
}

#[test]
fn balanced_workload_covers_owners_without_multiplying_pages() {
    for workers in [1, 2, 4, 5, 16, 128, 256] {
        let map = WorkerMap::new((0..workers as u16).map(WorkerId).collect()).unwrap();
        let keys = balanced_keys(&map, workers, REQUESTS + workers);
        assert_eq!(
            keys.iter().collect::<std::collections::BTreeSet<_>>().len(),
            keys.len()
        );
        for (index, key) in keys.iter().enumerate() {
            assert_eq!(
                map.metadata_owner(&object(*key)).unwrap(),
                WorkerId((index % workers) as u16)
            );
        }
        assert!(slab_bytes(workers) >= 128 * MIB);
    }
}

#[test]
fn automatic_and_capped_plans_preserve_shared_and_explicit_paired_crypto() {
    use uring_runtime::group::affinity::CpuLocation;
    let mut config = default_config();
    let topology = EffectiveTopology {
        cpus: (0..12)
            .map(|cpu| CpuLocation {
                cpu,
                package: 0,
                core: cpu,
                numa_node: Some(0),
            })
            .collect(),
        quota: None,
        nics: vec![],
    };
    for (cap, io, crypto) in [(usize::MAX, 8, 4), (8, 5, 3), (2, 1, 1)] {
        config.max_threads = cap;
        let plan = AffinityPlan::from_topology(&config, topology.clone(), &[]).unwrap();
        let owned = owned_plan(&plan);
        assert_eq!(owned.pairs.len(), io);
        assert_eq!(owned.crypto_groups().len(), crypto);
        assert_eq!(owned.max_threads, cap.saturating_add(1));
        assert!(io + crypto < owned.max_threads);
        assert_eq!(owned.crypto_groups(), plan.crypto_groups());
    }
    config.max_threads = 1;
    assert!(AffinityPlan::from_topology(&config, topology.clone(), &[]).is_err());
    config.max_threads = 2;
    // Legacy fixtures can still explicitly assign a different crypto CPU to
    // every I/O shard; shared execution is determined by CPU identity only.
    let mut paired = AffinityPlan::from_topology(&config, topology.clone(), &[]).unwrap();
    let first = paired.pairs[0].clone();
    paired.pairs = (0..4)
        .map(|worker| {
            let mut pair = first.clone();
            pair.worker = WorkerId(worker);
            pair.io = topology.cpus[usize::from(worker) * 2].clone();
            pair.crypto = topology.cpus[usize::from(worker) * 2 + 1].clone();
            pair
        })
        .collect();
    paired.max_threads = 8;
    assert_eq!(
        owned_plan(&paired).crypto_groups(),
        vec![vec![0], vec![1], vec![2], vec![3]]
    );
    assert_eq!(owned_plan(&paired).max_threads, 9);
}

#[test]
fn shared_directory_routes_cold_and_offline_reads_to_both_owners() {
    let mut plan = default_plan();
    // Even a one-CPU host can execute two I/O shards sharing one crypto thread.
    // This is a routing test, not a benchmark sizing override.
    let mut second = plan.pairs[0].clone();
    second.worker = WorkerId(1);
    plan.pairs.truncate(1);
    plan.pairs.push(second);
    plan.max_threads = 4; // Two I/O threads, one crypto thread, and the caller.
    assert_eq!(plan.crypto_groups(), vec![vec![0, 1]]);
    let ids = vec![WorkerId(0), WorkerId(1)];
    let map = Arc::new(WorkerMap::new(ids.clone()).unwrap());
    let keys = balanced_keys(&map, 2, 2);
    let scratch = Scratch::new();
    let mut receivers = BTreeMap::new();
    let commands: Vec<_> = ids
        .iter()
        .map(|worker| {
            let (send, receive) = mpsc::sync_channel(256);
            receivers.insert(worker.0, receive);
            send
        })
        .collect();
    let factory = Arc::new(Factory {
        resources: Resources::new(budgets(), ids.clone(), 1).unwrap(),
        root: scratch.path.clone(),
        commands: Mutex::new(receivers),
        directory: Arc::new(WorkerDirectory::new(map, ids, 256).unwrap()),
        slab_bytes: 256 * MIB,
    });
    let mut group = WorkerGroup::new(plan);
    group.start(factory, &scope()).unwrap();
    for (owner, key) in keys.iter().enumerate() {
        let mut client = connect(&commands[1 - owner]);
        assert_eq!(get(&mut client, *key, true).unwrap(), 200);
    }
    let initial = inspect_all(&commands, true);
    for stats in &initial {
        assert_eq!(stats.records, 1);
        assert!(stats.origin_calls > 0);
        assert_eq!(stats.requests, 1);
        assert!(stats.failures.is_empty());
    }
    for sender in &commands {
        sender.send(Command::Offline).unwrap();
    }
    // Inspect is an ordering fence for the preceding Offline command.
    inspect_all(&commands, false);
    for (owner, key) in keys.iter().enumerate() {
        let mut client = connect(&commands[1 - owner]);
        assert_eq!(get(&mut client, *key, true).unwrap(), 200);
    }
    for (owner, stats) in inspect_all(&commands, false).iter().enumerate() {
        assert_eq!(
            stats.origin_calls, initial[owner].origin_calls,
            "disk hit must stay on the original owner"
        );
        assert_eq!(stats.requests, 2);
        assert!(stats.failures.is_empty());
    }
    group.drain(&scope()).unwrap();
    group.shutdown(&scope()).unwrap();
    group.join().unwrap();
}

#[test]
#[ignore = "owns brd/ext4 via sudo; release-only, worker-sized RAM preflight"]
fn hotpath() {
    // This ignored benchmark must fail at runtime, not prevent debug builds.
    #[allow(clippy::assertions_on_constants)]
    {
        assert!(!cfg!(debug_assertions), "run with --release");
    }
    let plan = default_plan();
    let mut disk = Brd::new(plan.pairs.len(), &budgets(), slab_bytes(plan.pairs.len()));
    if std::env::var_os("RACER_BENCH_FAIL_SETUP").is_some() {
        panic!("requested cleanup probe");
    }
    for mode in ["memory", "disk", "fill"] {
        for concurrency in [1, 32, 128] {
            run_case(&disk.mount(), &plan, mode, concurrency);
        }
    }
    assert!(disk.close(), "brd teardown failed");
}

fn run_case(root: &std::path::Path, default: &AffinityPlan, mode: &str, concurrency: usize) {
    let workers = default.pairs.len();
    preflight(workers, &budgets(), slab_bytes(workers));
    let ids: Vec<_> = default.pairs.iter().map(|pair| pair.worker).collect();
    let map = Arc::new(WorkerMap::new(ids.clone()).unwrap());
    let keys = balanced_keys(&map, workers, REQUESTS + workers);
    let directory = Arc::new(WorkerDirectory::new(map, ids.clone(), 256).unwrap());
    let mut receivers = BTreeMap::new();
    let commands: Vec<_> = ids
        .iter()
        .map(|worker| {
            let (send, receive) = mpsc::sync_channel(256);
            receivers.insert(worker.0, receive);
            send
        })
        .collect();
    // Owned startup needs a slot for the benchmark driver (worker.rs::start).
    // Preserve the production-selected assignments/CPUs; this adds no worker.
    let plan = owned_plan(default);
    let crypto_threads = plan.crypto_groups().len();
    let factory = Arc::new(Factory {
        resources: Resources::new(budgets(), ids.clone(), 1).unwrap(),
        root: root.into(),
        commands: Mutex::new(receivers),
        directory,
        slab_bytes: slab_bytes(workers),
    });
    let mut group = WorkerGroup::new(plan);
    group.start(factory, &scope()).unwrap();
    println!(
        "case={mode} concurrency={concurrency} io_shards={workers} crypto_threads={crypto_threads} worker_threads={} driver_threads=1 plaintext_MiB_per_worker=256 ciphertext_MiB_per_worker=512 dirty_MiB_per_worker=128 page_MiB=16",
        workers + crypto_threads
    );
    // Warm connections/crypto and fully validate fixture bytes outside timing.
    let warm_count = if mode == "disk" { REQUESTS } else { workers };
    for (index, key) in keys.iter().take(warm_count).enumerate() {
        let mut warm = connect(&commands[(index + 1) % workers]);
        assert_eq!(get(&mut warm, *key, true).unwrap(), 200);
        inspect_all(&commands, false);
    }
    let initial = inspect_all(&commands, mode == "disk");
    assert_eq!(initial.iter().map(|s| s.records).sum::<usize>(), warm_count);
    for (owner, stats) in initial.iter().enumerate() {
        let expected = (owner..warm_count).step_by(workers).count();
        assert_eq!(stats.records, expected, "preload must reach its real owner");
        assert_eq!(stats.origin_calls > 0, expected > 0);
    }
    if mode != "fill" {
        for commands in &commands {
            commands.send(Command::Offline).unwrap();
        }
    }
    let mut samples = Vec::new();
    let mut failures = BTreeMap::<String, usize>::new();
    let mut elapsed = Duration::ZERO;
    let mut persisted_time = Duration::ZERO;
    for sweep in 0..if mode == "fill" { 1 } else { SWEEPS } {
        if mode == "disk" {
            inspect_all(&commands, true);
        }
        let barrier = Barrier::new(concurrency + 1);
        let start = thread::scope(|threads| {
            let mut handles = Vec::new();
            for client_index in 0..concurrency {
                let commands = &commands[client_index % workers];
                let keys = &keys;
                let barrier = &barrier;
                handles.push(threads.spawn(move || {
                    let mut samples = Vec::new();
                    barrier.wait();
                    barrier.wait();
                    for index in (client_index..REQUESTS).step_by(concurrency) {
                        // V2 completion closes each subscription after success.
                        let mut client = connect(commands);
                        let key = match mode {
                            "memory" => keys[(index + 1) % workers],
                            "disk" => keys[(index + 1) % REQUESTS],
                            _ => keys[workers + (index + 1) % REQUESTS],
                        };
                        let start = Instant::now();
                        let status = get(&mut client, key, false);
                        samples.push((status, start.elapsed()));
                    }
                    samples
                }));
            }
            barrier.wait();
            let start = Instant::now();
            barrier.wait();
            for handle in handles {
                for (status, latency) in handle.join().unwrap() {
                    match status {
                        Ok(200) => samples.push(latency),
                        Ok(status) => *failures.entry(format!("HTTP-{status}")).or_default() += 1,
                        Err(error) => {
                            *failures.entry(format!("{:?}", error.kind())).or_default() += 1
                        }
                    }
                }
            }
            start
        });
        elapsed += start.elapsed();
        let stats = inspect_all(&commands, false);
        persisted_time += start.elapsed();
        for (worker, stats) in stats.iter().enumerate() {
            println!(
                "  sweep={sweep} worker={worker} resident_records={} dirty_discards={} origin_calls={} ingress_requests={} server_failures={:?}",
                stats.records, stats.discarded, stats.origin_calls, stats.requests, stats.failures
            );
            if mode != "fill" {
                assert_eq!(
                    stats.origin_calls, initial[worker].origin_calls,
                    "cache workload contacted origin"
                );
            }
        }
        if mode == "fill" {
            println!(
                "  fresh_pages_persisted={}/{} (drained does not mean all responses persisted)",
                stats.iter().map(|s| s.records).sum::<usize>() - warm_count,
                REQUESTS
            );
        }
    }
    samples.sort_unstable();
    assert!(!samples.is_empty(), "no successful reads");
    let percentile = |p: usize| {
        samples[((samples.len() * p).div_ceil(100)).saturating_sub(1)].as_secs_f64() * 1000.
    };
    println!(
        "{mode} c={concurrency} ok={} failures={failures:?} req/s={:.1} MiB/s={:.1} p50_ms={:.3} p95_ms={:.3} p99_ms={:.3} read_s={:.3} drained_s={:.3}",
        samples.len(),
        samples.len() as f64 / elapsed.as_secs_f64(),
        samples.len() as f64 * 16. / elapsed.as_secs_f64(),
        percentile(50),
        percentile(95),
        percentile(99),
        elapsed.as_secs_f64(),
        persisted_time.as_secs_f64()
    );
    group.drain(&scope()).unwrap();
    group.shutdown(&scope()).unwrap();
    group.join().unwrap();
}

// Exclusively owned RAM disk. All mountpoints and device nodes are in target/.
fn device_bytes(workers: usize, slab_bytes: u64) -> u64 {
    (4096 * MIB).max(slab_bytes * workers as u64 + 1024 * MIB)
}

fn memory_budget(workers: usize, limits: &Limits, slab_bytes: u64) -> (u64, u64) {
    // Resident plaintext/cache, ciphertext including slab/crypto staging, dirty
    // retention, registered allowance (unused here), and request contexts. Some
    // dimensions overlap; adding them is deliberately conservative.
    let admitted = [
        limits.plaintext_bytes,
        limits.ciphertext_bytes,
        limits.dirty_bytes,
        limits.registered_bytes,
        limits.request_context_bytes,
    ]
    .iter()
    .map(|n| n.get() as u64)
    .sum::<u64>()
        * workers as u64;
    // Per worker: streaming origin (8 connections, 64 KiB chunks), stacks,
    // crypto scratch, queues/indexes, rings, sockets and pipes. Origin never
    // materializes all payloads. Global: allocator retention plus 128 clients'
    // stacks, two 64 KiB buffers apiece and kernel socket buffers.
    let auxiliary = workers as u64 * 512 * MIB + 2048 * MIB + 512 * MIB;
    let envelope = device_bytes(workers, slab_bytes) + admitted + auxiliary;
    (envelope, (16 << 30).max(envelope * 2))
}

fn preflight(workers: usize, limits: &Limits, slab_bytes: u64) {
    assert!(workers > 0);
    let device = device_bytes(workers, slab_bytes);
    assert!(
        slab_bytes * workers as u64 <= device - (1 << 30),
        "aggregate slab capacity must leave 1 GiB for ext4/headroom"
    );
    let (envelope, required) = memory_budget(workers, limits, slab_bytes);
    let info = fs::read_to_string("/proc/meminfo").unwrap();
    let available = info
        .lines()
        .find(|s| s.starts_with("MemAvailable:"))
        .unwrap()
        .split_whitespace()
        .nth(1)
        .unwrap()
        .parse::<u64>()
        .unwrap()
        * 1024;
    println!(
        "memory_preflight workers={workers} device_MiB={} slab_capacity_MiB={} plaintext_total_MiB={} ciphertext_total_MiB={} dirty_total_MiB={} context_total_MiB={} auxiliary_MiB={} envelope_MiB={} required_available_MiB={} host_available_MiB={}",
        device / MIB,
        slab_bytes * workers as u64 / MIB,
        limits.plaintext_bytes.get() as u64 * workers as u64 / MIB,
        limits.ciphertext_bytes.get() as u64 * workers as u64 / MIB,
        limits.dirty_bytes.get() as u64 * workers as u64 / MIB,
        limits.request_context_bytes.get() as u64 * workers as u64 / MIB,
        workers as u64 * 512 + 2560,
        envelope / MIB,
        required.div_ceil(MIB),
        available / MIB
    );
    assert!(
        available >= required,
        "insufficient host RAM; refusing benchmark"
    );
    // Fail closed on layouts we cannot resolve, rather than treating a missing
    // memory.max as unlimited. This fixture supports unified cgroup v2 mounted
    // at its namespace root; it does not silently skip v1 or relocated mounts.
    let mounts = fs::read_to_string("/proc/self/mountinfo").unwrap();
    assert!(
        mounts.lines().any(|line| {
            let Some((before, after)) = line.split_once(" - ") else {
                return false;
            };
            let fields: Vec<_> = before.split_whitespace().collect();
            after.starts_with("cgroup2 ")
                && fields.get(3) == Some(&"/")
                && fields.get(4) == Some(&"/sys/fs/cgroup")
        }),
        "benchmark requires a resolvable unified cgroup-v2 mount"
    );
    let groups = fs::read_to_string("/proc/self/cgroup").unwrap();
    let group = groups
        .lines()
        .find_map(|s| s.strip_prefix("0::"))
        .expect("benchmark requires cgroup v2");
    assert!(!group.split('/').any(|part| part == ".."));
    let root = std::path::Path::new("/sys/fs/cgroup");
    let leaf = root.join(group.trim_start_matches('/'));
    assert!(leaf.is_dir(), "unresolved cgroup membership");
    for path in leaf.ancestors().take_while(|path| path.starts_with(root)) {
        if path == root {
            break;
        } // The root cgroup has no memory controller files.
        let max =
            fs::read_to_string(path.join("memory.max")).expect("cannot check cgroup memory limit");
        let used = fs::read_to_string(path.join("memory.current"))
            .unwrap()
            .trim()
            .parse::<u64>()
            .unwrap();
        println!(
            "cgroup_memory path={} max={} current_MiB={}",
            path.display(),
            max.trim(),
            used / MIB
        );
        if max.trim() != "max" {
            let max = max.trim().parse::<u64>().unwrap();
            assert!(
                max.saturating_sub(used) >= required,
                "insufficient cgroup RAM; refusing benchmark"
            );
        }
    }
}

struct Brd {
    root: PathBuf,
    _lock: fs::File,
    loaded: bool,
    mounted: bool,
}

fn privileged(args: &[&str]) {
    let status = std::process::Command::new("sudo")
        .arg("-n")
        .args(args)
        .status()
        .unwrap();
    assert!(status.success(), "sudo {args:?}: {status}");
}

impl Brd {
    fn new(workers: usize, limits: &Limits, slab_bytes: u64) -> Self {
        preflight(workers, limits, slab_bytes);
        let target = PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("target");
        fs::create_dir_all(&target).unwrap();
        let lock = fs::OpenOptions::new()
            .create(true)
            .truncate(false)
            .write(true)
            .open(target.join("hotpath.lock"))
            .unwrap();
        // SAFETY: live descriptor, advisory exclusive lock released by File::drop.
        assert_eq!(
            unsafe { libc::flock(lock.as_raw_fd(), libc::LOCK_EX | libc::LOCK_NB) },
            0,
            "another benchmark owns brd"
        );
        assert!(
            !std::path::Path::new("/sys/module/brd").exists(),
            "brd already loaded; refusing to touch existing RAM disks"
        );
        let root = target.join(format!("hotpath-brd-{}", std::process::id()));
        fs::create_dir(&root).unwrap();
        let mut fixture = Self {
            root,
            _lock: lock,
            loaded: false,
            mounted: false,
        };
        fs::create_dir(fixture.root.join("ext4")).unwrap();
        privileged(&[
            "modprobe",
            "brd",
            "rd_nr=1",
            &format!("rd_size={}", device_bytes(workers, slab_bytes) / 1024),
            "max_part=1",
        ]);
        fixture.loaded = true;
        let device = fixture.root.join("device");
        privileged(&["mknod", device.to_str().unwrap(), "b", "1", "0"]);
        privileged(&[
            "mkfs.ext4",
            "-q",
            "-m",
            "0",
            "-E",
            "lazy_itable_init=0,lazy_journal_init=0",
            device.to_str().unwrap(),
        ]);
        privileged(&[
            "mount",
            "-t",
            "ext4",
            "-o",
            "noatime",
            device.to_str().unwrap(),
            fixture.mount().to_str().unwrap(),
        ]);
        fixture.mounted = true;
        // SAFETY: getuid/getgid have no preconditions.
        let owner = unsafe { format!("{}:{}", libc::getuid(), libc::getgid()) };
        privileged(&["chown", &owner, fixture.mount().to_str().unwrap()]);
        fixture
    }
    fn mount(&self) -> PathBuf {
        self.root.join("ext4")
    }
    fn close(&mut self) -> bool {
        if self.mounted {
            let ok = std::process::Command::new("sudo")
                .args(["-n", "umount"])
                .arg(self.mount())
                .status()
                .is_ok_and(|s| s.success());
            if !ok {
                return false;
            }
            self.mounted = false;
        }
        if self.loaded {
            let ok = std::process::Command::new("sudo")
                .args(["-n", "modprobe", "-r", "brd"])
                .status()
                .is_ok_and(|s| s.success());
            if !ok {
                return false;
            }
            self.loaded = false;
        }
        fs::remove_dir_all(&self.root).is_ok()
    }
}

#[test]
fn memory_envelope_scales_with_workers_and_retains_headroom() {
    let limits = budgets();
    let (one, one_required) = memory_budget(1, &limits, slab_bytes(1));
    let (four, four_required) = memory_budget(4, &limits, slab_bytes(4));
    assert!(four > one);
    assert!(one_required >= 16 << 30);
    assert_eq!(four_required, four * 2);
    assert_eq!(four, 12416 * MIB + 4 * 16);
    for workers in [5, 16, 128, 256] {
        let slab = slab_bytes(workers);
        assert!(device_bytes(workers, slab) >= slab * workers as u64 + 1024 * MIB);
        let (envelope, required) = memory_budget(workers, &limits, slab);
        assert!(envelope > four);
        assert_eq!(required, envelope * 2);
    }
}
impl Drop for Brd {
    fn drop(&mut self) {
        if self.root.exists() && !self.close() {
            eprintln!("brd cleanup failed: {}", self.root.display());
        }
    }
}
use uring_runtime::group::affinity::EffectiveTopology;
