// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Standalone HTTP transport benchmark using 4 MiB file or immutable-buffer bodies.
//! Run `http-bench --help` for server/client options. Select disjoint physical cores
//! for each process; file mode needs an existing ext4 slab directory.
use racer_dataplane::{
    allocator::{Allocator, Slab},
    buffers::{self, BUFFER_SIZE, Fill, Key},
    cache::CachedValue,
    http_client as client, http_server as server, tls, uring, workers,
};
use std::{
    env, io,
    net::SocketAddr,
    num::{NonZeroU32, NonZeroUsize},
    sync::{
        Arc, Mutex, OnceLock,
        atomic::{AtomicBool, Ordering},
    },
    thread,
    time::{Duration, Instant},
};

const BUDGET: usize = 128;
const REQUEST_TIMEOUT: Duration = Duration::from_secs(30);
static STOP: AtomicBool = AtomicBool::new(false);

fn invalid(message: &str) -> io::Error {
    io::Error::new(io::ErrorKind::InvalidInput, message)
}

struct Options {
    server: bool,
    address: SocketAddr,
    connections: usize,
    warmup: Duration,
    duration: Duration,
    body: String,
    slab_dir: std::path::PathBuf,
    tls_trust_dir: Option<std::path::PathBuf>,
    tls_cert: Option<std::path::PathBuf>,
    tls_key: Option<std::path::PathBuf>,
    tls_peer: Option<String>,
}
impl Options {
    fn parse() -> io::Result<Option<Self>> {
        let mut args = env::args().skip(1);
        let mode = args.next().unwrap_or_default();
        if mode.is_empty() || mode == "--help" {
            println!(
                "http-bench server [--listen IP:PORT] [--body file|buffer] [--slab-dir EXT4_DIRECTORY]\nhttp-bench client [--connect IP:PORT] [--connections-per-worker N] [--warmup SECONDS] [--duration SECONDS]\nBoth modes: --tls-trust-dir DIR --tls-cert PEM --tls-key PEM --tls-peer SPIFFE_URI enables mutual TLS and automatic kTLS. All four TLS options are required together.\nUse taskset to select workers (one per allowed physical core). Payload: 4 MiB; default body: file."
            );
            return Ok(None);
        }
        let server = match mode.as_str() {
            "server" => true,
            "client" => false,
            _ => return Err(invalid("expected server or client")),
        };
        let mut options = Self {
            server,
            address: "127.0.0.1:8080".parse().unwrap(),
            connections: 8,
            warmup: Duration::from_secs(5),
            duration: Duration::from_secs(30),
            body: "file".into(),
            slab_dir: env::temp_dir(),
            tls_trust_dir: None,
            tls_cert: None,
            tls_key: None,
            tls_peer: None,
        };
        while let Some(flag) = args.next() {
            let value = args.next().ok_or_else(|| invalid("missing option value"))?;
            match flag.as_str() {
                "--tls-trust-dir" => options.tls_trust_dir = Some(value.into()),
                "--tls-cert" => options.tls_cert = Some(value.into()),
                "--tls-key" => options.tls_key = Some(value.into()),
                "--tls-peer" => options.tls_peer = Some(value),
                "--body" if server && matches!(value.as_str(), "file" | "buffer") => {
                    options.body = value
                }
                "--slab-dir" if server => options.slab_dir = value.into(),
                "--listen" if server => {
                    options.address = value
                        .parse()
                        .map_err(|_| invalid("invalid listen address"))?;
                }
                "--connect" if !server => {
                    options.address = value
                        .parse()
                        .map_err(|_| invalid("invalid connect address"))?;
                }
                "--connections-per-worker" if !server => {
                    options.connections =
                        value.parse().map_err(|_| invalid("invalid connections"))?;
                    if !(1..=128).contains(&options.connections) {
                        return Err(invalid("connections per worker must be in 1..=128"));
                    }
                }
                "--warmup" | "--duration" if !server => {
                    let seconds: u64 = value.parse().map_err(|_| invalid("invalid seconds"))?;
                    if !(1..=3600).contains(&seconds) {
                        return Err(invalid("seconds must be in 1..=3600"));
                    }
                    if flag == "--warmup" {
                        options.warmup = Duration::from_secs(seconds);
                    } else {
                        options.duration = Duration::from_secs(seconds);
                    }
                }
                _ => return Err(invalid("unknown option for this mode")),
            }
        }
        if options.address.port() == 0 {
            return Err(invalid("port must be nonzero"));
        }
        let tls_options = [
            options.tls_trust_dir.is_some(),
            options.tls_cert.is_some(),
            options.tls_key.is_some(),
            options.tls_peer.is_some(),
        ];
        if tls_options.iter().any(|enabled| *enabled) && !tls_options.iter().all(|enabled| *enabled)
        {
            return Err(invalid("all four TLS options are required together"));
        }
        Ok(Some(options))
    }

    fn tls(&self) -> io::Result<Option<(tls::TlsContext, tls::ExpectedPeer)>> {
        let Some(dir) = &self.tls_trust_dir else {
            return Ok(None);
        };
        let bundle = tls::TrustBundle::load(dir, None)?;
        let certificate = std::fs::read(self.tls_cert.as_ref().unwrap())?;
        let key = std::fs::read(self.tls_key.as_ref().unwrap())?;
        let expected = tls::PeerIdentity::parse(self.tls_peer.as_ref().unwrap())?;
        Ok(Some((
            tls::TlsContext::new(&bundle, &certificate, &key)?,
            tls::ExpectedPeer::Identity(expected),
        )))
    }
}

fn pattern(index: usize) -> u8 {
    (index as u8)
        .wrapping_mul(31)
        .wrapping_add((index >> 12) as u8)
}

fn fill(pool: &buffers::WorkerPool, worker: usize, connection: usize) -> io::Result<Fill> {
    let mut key = [0; 32];
    key[..8].copy_from_slice(&(worker as u64).to_le_bytes());
    key[8..16].copy_from_slice(&(connection as u64).to_le_bytes());
    pool.stage(Key::new(key))
        .map_err(|_| io::Error::other("pool exhausted"))
}

// Keep affine transport state inline rather than allocating another box per GET.
#[allow(clippy::large_enum_variant)]
enum Task {
    Request(server::Request),
    Headers(server::SendingGetHeaders),
    Head(server::SendingHeadHeaders),
    Body(server::SendingBody),
    Finished,
}
struct Handler {
    payload: CachedValue,
    failed: bool,
}
impl Handler {
    fn progress(
        &mut self,
        task: &mut Task,
        ring: &mut uring::Ring,
        budget: usize,
    ) -> io::Result<server::Progress<server::Completed>> {
        let progress = match task {
            Task::Request(_) => {
                let Task::Request(request) = std::mem::replace(task, Task::Finished) else {
                    unreachable!()
                };
                let head = server::ResponseHead::new(200, Some(BUFFER_SIZE as u64), &[])?;
                *task = match request {
                    server::Request::Get(request) => Task::Headers(request.respond(head)?),
                    server::Request::Head(request) => Task::Head(request.respond(head)?),
                };
                return Ok(server::Progress::Pending(uring::Work {
                    runnable: true,
                    deadline: None,
                }));
            }
            Task::Headers(send) => send.poll(ring, budget)?,
            Task::Body(send) => send.poll(ring, budget)?,
            Task::Head(send) => return send.poll(ring, budget),
            Task::Finished => return Err(io::Error::other("finished server task polled")),
        };
        match progress {
            server::Progress::Pending(work) => Ok(server::Progress::Pending(work)),
            server::Progress::Ready(server::BodyProgress::Done(done)) => {
                *task = Task::Finished;
                Ok(server::Progress::Ready(done))
            }
            server::Progress::Ready(server::BodyProgress::More(writer)) => {
                let chunk = server::BodyChunk::value(self.payload.clone(), 0..BUFFER_SIZE)
                    .map_err(|error| error.error)?;
                *task = Task::Body(writer.send(chunk).map_err(|error| error.error)?);
                Ok(server::Progress::Pending(uring::Work {
                    runnable: true,
                    deadline: None,
                }))
            }
        }
    }
}
impl server::Handler for Handler {
    type Task = Task;
    fn start(&mut self, request: server::Request) -> Task {
        Task::Request(request)
    }
    fn poll(
        &mut self,
        task: &mut Task,
        ring: &mut uring::Ring,
        budget: usize,
    ) -> io::Result<server::Progress<server::Completed>> {
        let result = self.progress(task, ring, budget);
        self.failed |= result.is_err();
        result
    }
}

#[derive(Clone, Copy)]
struct Window {
    warmup: Instant,
    start: Instant,
    end: Instant,
}
struct Slot {
    idle: Option<(client::Connection, Fill)>,
    exchange: Option<client::GetExchange>,
    started: Instant,
    verified: bool,
}
#[derive(Default)]
struct Row {
    worker: usize,
    completed: u64,
    verified: usize,
}
struct Client {
    slots: Vec<Slot>,
    first: usize,
    window: Arc<OnceLock<Window>>,
    row: Row,
    results: Arc<Mutex<Vec<Row>>>,
    draining: bool,
}
impl Client {
    fn poll(&mut self, ring: &mut uring::Ring, budget: usize) -> io::Result<uring::Work> {
        let Some(window) = self.window.get().copied() else {
            return Ok(uring::Work {
                runnable: true,
                deadline: None,
            });
        };
        if Instant::now() < window.warmup {
            return Ok(uring::Work {
                runnable: false,
                deadline: Some(window.warmup),
            });
        }
        let mut work = uring::Work::default();
        let exchange_budget = (budget / self.slots.len()).max(1);
        // Bound the scheduler batch, rotate the first slot, and merge every pending
        // exchange's deadline before allowing the production driver to sleep.
        for offset in 0..self.slots.len() {
            let index = (self.first + offset) % self.slots.len();
            let slot = &mut self.slots[index];
            if let Some(exchange) = &mut slot.exchange {
                match exchange.poll(ring, exchange_budget)? {
                    client::Progress::Pending(pending) => work.merge(pending),
                    client::Progress::Ready(mut response) => {
                        let completed = Instant::now();
                        if response.status() != 200
                            || response.content_length() != Some(BUFFER_SIZE as u64)
                        {
                            return Err(io::Error::other(
                                "unexpected HTTP status or Content-Length",
                            ));
                        }
                        if !slot.verified {
                            if completed >= window.start {
                                return Err(io::Error::other(
                                    "connection did not validate during warmup; increase --warmup",
                                ));
                            }
                            if response
                                .body()
                                .iter()
                                .enumerate()
                                .any(|(i, &byte)| byte != pattern(i))
                            {
                                return Err(io::Error::other("payload validation failed"));
                            }
                            if Instant::now() >= window.start {
                                return Err(io::Error::other(
                                    "payload validation exceeded warmup; increase --warmup",
                                ));
                            }
                            slot.verified = true;
                            self.row.verified += 1;
                        }
                        let (connection, fill, length) = response.recycle();
                        if length != BUFFER_SIZE {
                            return Err(io::Error::other("short HTTP body"));
                        }
                        let connection = connection
                            .ok_or_else(|| io::Error::other("connection closed unexpectedly"))?;
                        if slot.started >= window.start && completed < window.end {
                            self.row.completed += 1;
                        }
                        slot.exchange = None;
                        slot.idle = Some((connection, fill));
                    }
                }
            }
            let now = Instant::now();
            if !self.draining && now < window.end && slot.exchange.is_none() {
                let (connection, fill) = slot.idle.take().unwrap();
                slot.started = now;
                slot.exchange = Some(connection.get(
                    client::Request::new("/payload", &[])?,
                    fill,
                    now + REQUEST_TIMEOUT,
                )?);
                work.runnable = true;
            }
        }
        self.first = (self.first + 1) % self.slots.len();
        Ok(work)
    }
    fn shutdown(&mut self, ring: &mut uring::Ring) -> io::Result<()> {
        self.draining = true;
        let deadline = Instant::now() + REQUEST_TIMEOUT;
        while self.slots.iter().any(|slot| slot.exchange.is_some()) {
            if Instant::now() >= deadline {
                return Err(io::Error::new(
                    io::ErrorKind::TimedOut,
                    "client drain timed out",
                ));
            }
            ring.progress()?;
            let work = self.poll(ring, BUDGET)?;
            if !work.runnable && self.slots.iter().any(|slot| slot.exchange.is_some()) {
                ring.wait(Some(work.deadline.map_or(deadline, |d| d.min(deadline))))?;
            }
        }
        if self.row.verified != self.slots.len() || self.row.completed == 0 {
            return Err(io::Error::other(
                "worker did not validate all connections or measure any responses",
            ));
        }
        self.slots.clear();
        self.results
            .lock()
            .unwrap()
            .push(std::mem::take(&mut self.row));
        Ok(())
    }
}

enum App {
    Server(server::Server<Handler>),
    Client(Client),
}
impl uring::Application for App {
    fn poll(&mut self, ring: &mut uring::Ring, budget: usize) -> io::Result<uring::Work> {
        match self {
            Self::Server(server) => {
                let work = server.poll(ring, budget)?;
                if server.handler().failed {
                    return Err(io::Error::other("server handler failed"));
                }
                Ok(work)
            }
            Self::Client(client) => client.poll(ring, budget),
        }
    }
    fn shutdown(&mut self, ring: &mut uring::Ring) -> io::Result<()> {
        match self {
            Self::Server(server) => server.shutdown(ring),
            Self::Client(client) => client.shutdown(ring),
        }
    }
}

extern "C" fn stop_signal(_: libc::c_int) {
    STOP.store(true, Ordering::Relaxed);
}
fn install_signals() -> io::Result<()> {
    // SAFETY: initialized sigaction; the handler only stores a lock-free atomic.
    unsafe {
        let mut action: libc::sigaction = std::mem::zeroed();
        action.sa_sigaction = stop_signal as *const () as usize;
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
    let Some(options) = Options::parse()? else {
        return Ok(());
    };
    if cfg!(debug_assertions) {
        return Err(invalid("build with --release"));
    }
    install_signals()?;
    let tls = options.tls()?;
    // An upper bound if SMT siblings are allowed; the production planner selects
    // just one CPU per physical core. taskset makes this bound exact in our runs.
    let allowed = thread::available_parallelism()?.get();
    let buffers = allowed
        .checked_mul(if options.server {
            1
        } else {
            options.connections
        })
        .filter(|&count| count <= 65536)
        .ok_or_else(|| invalid("too many buffers"))?;
    eprintln!(
        "pool_buffers_per_node={buffers} pool_bytes_per_node={} (registered by each worker on that node; sufficient memlock required)",
        buffers * BUFFER_SIZE
    );
    let pools = buffers::Pools::new(buffers::Config {
        buffers_per_node: NonZeroUsize::new(buffers).unwrap(),
        ..buffers::Config::new(NonZeroUsize::new(buffers).unwrap())
    });
    let window = Arc::new(OnceLock::new());
    let results = Arc::new(Mutex::new(Vec::new()));
    let factory_window = window.clone();
    let factory_results = results.clone();
    let slab = if options.server && options.body == "file" {
        let path = options
            .slab_dir
            .join(format!("racer-http-bench-{}.slab", std::process::id()));
        let slab = Slab::create(&path, allowed as u64 * 32 * 1024 * 1024, allowed)?;
        std::fs::remove_file(path)?;
        Some(Mutex::new(slab))
    } else {
        None
    };
    let group = workers::Workers::start(
        workers::Config {
            shard_count: NonZeroUsize::new(allowed).unwrap(),
        },
        move |placement| {
            let pool = pools.for_worker(placement)?;
            let worker = placement.worker_id().0;
            let mut ring = uring::Ring::new(
                placement,
                pool.clone(),
                uring::Config {
                    fixed_files: if options.server {
                        1024
                    } else {
                        options.connections as u32
                    },
                    ..Default::default()
                },
            )?;
            let app = if options.server {
                let mut fill = fill(&pool, worker, 0)?;
                for (i, byte) in fill.as_mut_slice().iter_mut().enumerate() {
                    *byte = pattern(i);
                }
                let payload = fill.publish(BUFFER_SIZE)?;
                let payload = if let Some(slab) = &slab {
                    let shard = slab.lock().unwrap().take_shard(placement.shard_ids()[0])?;
                    let mut allocator = Allocator::open(placement, shard, Default::default())?;
                    allocator
                        .insert_payload([0; 32], payload, None)
                        .map_err(|e| e.error)?;
                    let lease = allocator.lookup(&[0; 32], 0).unwrap();
                    while !allocator.is_idle() {
                        ring.progress()?;
                        let work = allocator.poll(&mut ring, BUDGET)?;
                        if !work.runnable && !allocator.is_idle() {
                            ring.wait(Some(Instant::now() + REQUEST_TIMEOUT))?;
                        }
                    }
                    CachedValue::File(lease.ready().unwrap())
                } else {
                    CachedValue::Buffer(payload)
                };
                let mut listener =
                    server::Listener::bind(options.address, NonZeroU32::new(1024).unwrap())?;
                if let Some((context, expected)) = &tls {
                    listener.set_tls(context.clone(), expected.clone());
                }
                App::Server(server::Server::new(
                    listener,
                    Handler {
                        payload,
                        failed: false,
                    },
                    server::Config::default(),
                ))
            } else {
                let mut slots = Vec::with_capacity(options.connections);
                for index in 0..options.connections {
                    let mut fill = fill(&pool, worker, index)?;
                    fill.as_mut_slice().fill(0);
                    slots.push(Slot {
                        idle: Some((
                            match &tls {
                                Some((context, expected)) => client::Connection::new_tls(
                                    options.address,
                                    &options.address.to_string(),
                                    context,
                                    expected.clone(),
                                )?,
                                None => client::Connection::new(
                                    options.address,
                                    &options.address.to_string(),
                                )?,
                            },
                            fill,
                        )),
                        exchange: None,
                        started: Instant::now(),
                        verified: false,
                    });
                }
                App::Client(Client {
                    slots,
                    first: 0,
                    window: factory_window.clone(),
                    row: Row {
                        worker,
                        ..Default::default()
                    },
                    results: factory_results.clone(),
                    draining: false,
                })
            };
            uring::Driver::new(ring, app, BUDGET)
        },
    )?;
    let worker_count = group.placements().len();
    for p in group.placements() {
        eprintln!(
            "worker={} cpu={} numa={}",
            p.worker_id().0,
            p.cpu_id().0,
            p.numa_node_id().0
        );
    }
    let warmup = Instant::now() + Duration::from_millis(100);
    let interval = Window {
        warmup,
        start: warmup + options.warmup,
        end: warmup + options.warmup + options.duration,
    };
    window.set(interval).ok().unwrap();
    eprintln!(
        "READY mode={} address={} workers={worker_count} connections_per_worker={} payload_bytes={BUFFER_SIZE}",
        if options.server { "server" } else { "client" },
        options.address,
        options.connections
    );
    let finished = Arc::new(AtomicBool::new(false));
    let done = finished.clone();
    let stop = group.stop_handle();
    let monitor = thread::spawn(move || {
        while !done.load(Ordering::Acquire) {
            if STOP.load(Ordering::Relaxed) || (!options.server && Instant::now() >= interval.end) {
                stop.request_stop();
                break;
            }
            thread::park_timeout(Duration::from_millis(10));
        }
    });
    let result = group.join();
    finished.store(true, Ordering::Release);
    monitor.thread().unpark();
    monitor
        .join()
        .map_err(|_| io::Error::other("monitor panicked"))?;
    result?;
    let offload = tls::global_counters();
    eprintln!("TLS {offload:?}");
    if options.server {
        eprintln!("server stopped cleanly");
        return Ok(());
    }
    if STOP.load(Ordering::Relaxed) {
        return Err(io::Error::new(
            io::ErrorKind::Interrupted,
            "benchmark interrupted",
        ));
    }
    let mut rows = results.lock().unwrap();
    if rows.len() != worker_count {
        return Err(io::Error::other("missing worker results"));
    }
    rows.sort_by_key(|row| row.worker);
    let mut completed = 0;
    for row in rows.iter() {
        println!(
            "worker={} completed={} verified_connections={}",
            row.worker, row.completed, row.verified
        );
        completed += row.completed;
    }
    let bytes = completed * BUFFER_SIZE as u64;
    let seconds = options.duration.as_secs_f64();
    println!(
        "RESULT workers={worker_count} connections_per_worker={} payload_bytes={BUFFER_SIZE} warmup_seconds={} seconds={seconds:.3} completed={completed} bytes={bytes} requests_per_second={:.3} gbit_per_second={:.3} errors=0",
        options.connections,
        options.warmup.as_secs(),
        completed as f64 / seconds,
        bytes as f64 * 8.0 / seconds / 1e9
    );
    Ok(())
}
