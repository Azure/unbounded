//! Opt-in bottleneck probe, reusing the production graph fixture above.
use super::*;
use racer_dataplane::{
    config::{Config, DEFAULT_MAX_THREADS},
    runtime::{
        affinity::{AffinityPlan, EffectiveTopology},
        worker::{WorkerFactory, WorkerGroup, WorkerRuntime, WorkerService},
    },
};
use std::{
    collections::VecDeque,
    io::BufReader,
    sync::{Barrier, mpsc},
};

mod brd;

const REQUESTS: usize = 128;
const SWEEPS: usize = 4;
const MIB: u64 = 1 << 20;

fn default_plan() -> AffinityPlan {
    // Parse real defaults without inheriting ambient RACER_* overrides or loading files.
    let (config, _) = Config::from_lookup_with_fabric_ports(|name| {
        Ok(match name {
            "RACER_CLUSTER_ID" => Some(CLUSTER.into()),
            "RACER_CONTROL_ENDPOINT" => Some("https://controller.invalid:443".into()),
            _ => None,
        })
    })
    .unwrap();
    assert_eq!(config.max_threads, DEFAULT_MAX_THREADS);
    let topology = EffectiveTopology::discover().unwrap();
    println!(
        "allowed_cpus={:?} cpu_quota={:?} default_max_threads={DEFAULT_MAX_THREADS}",
        topology.cpus, topology.quota
    );
    let plan = AffinityPlan::from_topology(&config, topology, &[]).unwrap();
    // app::partition_limits' default, non-RDMA progress floors admit all <=4 pairs.
    let n = plan.pairs.len();
    assert!(n <= 4);
    assert!(config.limits.plaintext_bytes.get() / n >= 3 * P as usize);
    assert!(
        config.limits.ciphertext_bytes.get() / n
            >= 3 * (P as usize + 16) + racer_dataplane::store::format::MAX_HEADER_BYTES
    );
    assert!(config.limits.dirty_bytes.get() / n >= P as usize + 16);
    println!("host_default_pairs={n} layout={:?}", plan.pairs);
    plan
}

// A full encrypted page plus its header allows three records per 64 MiB segment.
// Include one warm page per owner and one free segment; aggregate sparse lengths
// stay <=3 GiB, so even complete allocation leaves space on the 4 GiB filesystem.
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
impl WorkerFactory for Factory {
    fn limits(&self) -> Limits {
        budgets()
    }
    fn build(&self, worker: WorkerId, runtime: WorkerRuntime) -> Result<Box<dyn WorkerService>> {
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
    fn build_crypto(&self, _: WorkerId, runtime: CryptoRuntime) -> Result<Box<dyn CryptoService>> {
        Ok(Box::new(PageCryptoEngine::new(runtime)))
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
                        let mut lease =
                            ConnectionLease::from_accepted(socket.into(), &rig.admission)?;
                        loop {
                            let scope = scope();
                            let received = rig.io.receive_head(lease, &scope).await?;
                            let request = RequestParser::new(32768)
                                .parse(&CacheId(CACHE.into()), received.value)?;
                            let kind = request.kind.clone();
                            requests.set(requests.get() + 1);
                            match rig.dispatcher.read(request, &scope).await {
                                Ok(response) => {
                                    rig.responses.validate(&kind, &response)?;
                                    let sent = rig
                                        .responses
                                        .send(received.connection, response, &scope)
                                        .await;
                                    if let Err(error) = &sent {
                                        *failures
                                            .borrow_mut()
                                            .entry(format!("body:{error:?}"))
                                            .or_default() += 1;
                                    }
                                    lease = sent?;
                                    if !lease.is_reusable() {
                                        // send can now return a fully written error
                                        // head when first-slice admission fails.
                                        *failures
                                            .borrow_mut()
                                            .entry("response:rejected-before-body".into())
                                            .or_default() += 1;
                                        return Ok(());
                                    }
                                }
                                Err(error) => {
                                    *failures
                                        .borrow_mut()
                                        .entry(format!("read:{error:?}"))
                                        .or_default() += 1;
                                    rig.responses
                                        .send_error(received.connection, error, &scope)
                                        .await?;
                                    return Ok(());
                                }
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
        if let Some(write) = task.as_mut() {
            if let Poll::Ready(result) = write.as_mut().poll(cx) {
                result?;
                *task = None;
            }
        }
        if task.is_none() && self.rig.writer.pending_count() > 0 {
            let writer = self.rig.writer.clone();
            *task = Some(Box::pin(async move {
                writer.progress(1, &scope()).await.map(|_| ())
            }));
            cx.waker().wake_by_ref();
        }
        if task.is_none() && self.rig.writer.is_idle() {
            if let Some((evict, reply)) = self.inspection.take() {
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
        }
        Ok(())
    }
}
impl WorkerService for Service {
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
        "GET /v1/objects/{key:064x} HTTP/1.1\r\nHost: racer\r\nAuthorization: fixture-credential\r\nRacer-Metadata: fixture-metadata\r\nIf-Match: \"v1\"\r\nRange: bytes=0-16777215\r\n\r\n"
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
    let length: usize = fields["content-length"].parse().unwrap();
    if status == 206 {
        assert_eq!(length, P as usize);
        assert_eq!(fields["etag"], "\"v1\"");
        assert_eq!(fields["content-range"], format!("bytes 0-{}/{P}", P - 1));
    } else {
        assert!(length <= 32768);
    }
    let mut scratch = [0; 64 << 10];
    let mut offset = 0;
    while offset < length {
        let n = scratch.len().min(length - offset);
        client.read_exact(&mut scratch[..n])?;
        if status == 206 {
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
    Ok(status)
}

#[test]
fn balanced_workload_covers_owners_without_multiplying_pages() {
    for workers in 1..=4 {
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
        assert!(slab_bytes(workers) * workers as u64 <= 3 << 30);
    }
}

#[test]
fn shared_directory_routes_cold_and_offline_reads_to_both_owners() {
    let mut plan = default_plan();
    // Even a one-CPU host can execute two fixture pairs sharing allowed CPUs.
    // This is a routing test, not a benchmark sizing override.
    let mut second = plan.pairs[0].clone();
    second.worker = WorkerId(1);
    plan.pairs.truncate(1);
    plan.pairs.push(second);
    plan.max_threads = 5;
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
        root: scratch.path.clone(),
        commands: Mutex::new(receivers),
        directory: Arc::new(WorkerDirectory::new(map, ids, 256).unwrap()),
        slab_bytes: 256 * MIB,
    });
    let mut group = WorkerGroup::new(plan);
    group.start(factory, &scope()).unwrap();
    for (owner, key) in keys.iter().enumerate() {
        let mut client = connect(&commands[1 - owner]);
        assert_eq!(get(&mut client, *key, true).unwrap(), 206);
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
        assert_eq!(get(&mut client, *key, true).unwrap(), 206);
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
    assert!(!cfg!(debug_assertions), "run with --release");
    let plan = default_plan();
    let mut disk = brd::Brd::new(plan.pairs.len(), &budgets(), slab_bytes(plan.pairs.len()));
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
    brd::preflight(workers, &budgets(), slab_bytes(workers));
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
    // Preserve the production-selected pairs/CPUs; this does not add a worker.
    let plan = AffinityPlan {
        pairs: default.pairs.clone(),
        max_threads: default.max_threads + 1,
    };
    let factory = Arc::new(Factory {
        root: root.into(),
        commands: Mutex::new(receivers),
        directory,
        slab_bytes: slab_bytes(workers),
    });
    let mut group = WorkerGroup::new(plan);
    group.start(factory, &scope()).unwrap();
    println!(
        "case={mode} concurrency={concurrency} pairs={workers} worker_threads={} driver_threads=1 plaintext_MiB_per_worker=256 ciphertext_MiB_per_worker=512 dirty_MiB_per_worker=128 page_MiB=16",
        workers * 2
    );
    // Warm connections/crypto and fully validate fixture bytes outside timing.
    let warm_count = if mode == "disk" { REQUESTS } else { workers };
    for (index, key) in keys.iter().take(warm_count).enumerate() {
        let mut warm = connect(&commands[(index + 1) % workers]);
        assert_eq!(get(&mut warm, *key, true).unwrap(), 206);
        inspect_all(&commands, false);
    }
    let initial = inspect_all(&commands, mode == "disk");
    assert_eq!(initial.iter().map(|s| s.records).sum::<usize>(), warm_count);
    for (owner, stats) in initial.iter().enumerate() {
        let expected = (owner..warm_count).step_by(workers).count();
        assert_eq!(stats.records, expected, "preload must reach its real owner");
        assert!(stats.origin_calls > 0);
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
                    let mut client = connect(commands);
                    let mut samples = Vec::new();
                    barrier.wait();
                    barrier.wait();
                    for index in (client_index..REQUESTS).step_by(concurrency) {
                        let key = match mode {
                            "memory" => keys[(index + 1) % workers],
                            "disk" => keys[(index + 1) % REQUESTS],
                            _ => keys[workers + (index + 1) % REQUESTS],
                        };
                        let start = Instant::now();
                        let status = get(&mut client, key, false);
                        let reconnect = !matches!(status, Ok(206));
                        samples.push((status, start.elapsed()));
                        if reconnect {
                            client = connect(commands);
                        }
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
                        Ok(206) => samples.push(latency),
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
