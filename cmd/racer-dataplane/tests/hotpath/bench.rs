//! Opt-in bottleneck probe, reusing the production graph fixture above.
use super::*;
use racer_dataplane::runtime::{
    affinity::{AffinityPlan, EffectiveTopology, WorkerPair},
    worker::{WorkerFactory, WorkerGroup, WorkerRuntime, WorkerService},
};
use std::{
    collections::VecDeque,
    io::BufReader,
    sync::{Barrier, mpsc},
};

mod brd;

const REQUESTS: usize = 128;
const SWEEPS: usize = 4;

#[derive(Debug)]
struct Stats {
    records: usize,
    discarded: u64,
    origin_calls: usize,
    failures: BTreeMap<String, usize>,
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
    commands: Mutex<Option<mpsc::Receiver<Command>>>,
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
    fn build(&self, _: WorkerId, runtime: WorkerRuntime) -> Result<Box<dyn WorkerService>> {
        let rig = Rc::new(Rig::assemble(
            P,
            false,
            16,
            8,
            Some((Scratch::under(&self.root), runtime)),
        ));
        Ok(Box::new(Service {
            rig,
            commands: self.commands.lock().unwrap().take().unwrap(),
            clients: VecDeque::new(),
            inspection: None,
            failures: Rc::new(RefCell::new(BTreeMap::new())),
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
                    self.clients.push_back(Box::pin(async move {
                        let mut lease =
                            ConnectionLease::from_accepted(socket.into(), &rig.admission)?;
                        loop {
                            let scope = scope();
                            let received = rig.io.receive_head(lease, &scope).await?;
                            let request = RequestParser::new(32768)
                                .parse(&CacheId(CACHE.into()), received.value)?;
                            let kind = request.kind.clone();
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

fn inspect(commands: &mpsc::Sender<Command>, evict: bool) -> Stats {
    let (reply, result) = mpsc::channel();
    commands.send(Command::Inspect { evict, reply }).unwrap();
    result.recv_timeout(TIMEOUT).unwrap()
}
fn connect(commands: &mpsc::Sender<Command>) -> BufReader<UnixStream> {
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
#[ignore = "owns brd/ext4 via sudo; release-only, needs 16 GiB available memory"]
fn hotpath() {
    assert!(!cfg!(debug_assertions), "run with --release");
    let mut disk = brd::Brd::new();
    if std::env::var_os("RACER_BENCH_FAIL_SETUP").is_some() {
        panic!("requested cleanup probe");
    }
    for mode in ["memory", "disk", "fill"] {
        for concurrency in [1, 32, 128] {
            run_case(&disk.mount(), mode, concurrency);
        }
    }
    assert!(disk.close(), "brd teardown failed");
}

fn run_case(root: &std::path::Path, mode: &str, concurrency: usize) {
    let topology = EffectiveTopology::discover().unwrap();
    let io = topology.cpus[0].clone();
    let crypto = topology
        .cpus
        .iter()
        .find(|cpu| (cpu.package, cpu.core) != (io.package, io.core))
        .unwrap_or(&io)
        .clone();
    let plan = AffinityPlan {
        pairs: vec![WorkerPair {
            worker: WorkerId(0),
            io: io.clone(),
            crypto: crypto.clone(),
            nic: None,
        }],
        max_threads: 3,
    };
    let (commands, receive) = mpsc::channel();
    let factory = Arc::new(Factory {
        root: root.into(),
        commands: Mutex::new(Some(receive)),
    });
    let mut group = WorkerGroup::new(plan);
    group.start(factory, &scope()).unwrap();
    eprintln!(
        "case={mode} concurrency={concurrency} pairs=1 io_cpu={} crypto_cpu={} plaintext_MiB=256 ciphertext_MiB=512 dirty_MiB=128 page_MiB=16",
        io.cpu, crypto.cpu
    );
    // Warm connections/crypto and fully validate fixture bytes outside timing.
    let mut warm = connect(&commands);
    for key in 0..if mode == "disk" { REQUESTS } else { 1 } {
        assert_eq!(get(&mut warm, key, true).unwrap(), 206);
        inspect(&commands, false);
    }
    drop(warm);
    let initial = inspect(&commands, mode == "disk");
    if mode == "disk" {
        assert_eq!(initial.records, REQUESTS);
    }
    if mode != "fill" {
        commands.send(Command::Offline).unwrap();
    }
    let mut samples = Vec::new();
    let mut failures = BTreeMap::<String, usize>::new();
    let mut elapsed = Duration::ZERO;
    let mut persisted_time = Duration::ZERO;
    for sweep in 0..if mode == "fill" { 1 } else { SWEEPS } {
        if mode == "disk" {
            inspect(&commands, true);
        }
        let barrier = Barrier::new(concurrency + 1);
        let start = thread::scope(|threads| {
            let mut handles = Vec::new();
            for worker in 0..concurrency {
                let commands = &commands;
                let barrier = &barrier;
                handles.push(threads.spawn(move || {
                    let mut client = connect(commands);
                    let mut samples = Vec::new();
                    barrier.wait();
                    barrier.wait();
                    for index in (worker..REQUESTS).step_by(concurrency) {
                        let key = match mode {
                            "memory" => 0,
                            "disk" => index,
                            _ => 1 + sweep * REQUESTS + index,
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
        let stats = inspect(&commands, false);
        persisted_time += start.elapsed();
        eprintln!(
            "  sweep={sweep} resident_records={} dirty_discards={} origin_calls={} server_failures={:?}",
            stats.records, stats.discarded, stats.origin_calls, stats.failures
        );
        if mode != "fill" {
            assert_eq!(
                stats.origin_calls, initial.origin_calls,
                "cache workload contacted origin"
            );
        }
        if mode == "fill" {
            eprintln!(
                "  fresh_pages_persisted={}/{} (drained does not mean all responses persisted)",
                stats.records - initial.records,
                REQUESTS
            );
        }
    }
    samples.sort_unstable();
    assert!(!samples.is_empty(), "no successful reads");
    let percentile = |p: usize| {
        samples[((samples.len() * p).div_ceil(100)).saturating_sub(1)].as_secs_f64() * 1000.
    };
    eprintln!(
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
