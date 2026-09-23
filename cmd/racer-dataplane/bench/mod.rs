// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Opt-in two-node transport fixtures. Never enable dev-bench in deployed images.
use crate::{
    buffers::{self, BUFFER_SIZE},
    uring, workers,
};
use std::{
    io,
    net::SocketAddr,
    num::NonZeroUsize,
    path::PathBuf,
    sync::{
        Arc, Mutex, OnceLock,
        atomic::{AtomicBool, AtomicUsize, Ordering},
    },
    thread,
    time::{Duration, Instant},
};
mod fixture;
#[path = "rdma_transport.rs"]
mod rdma;
mod tcp;
#[cfg(test)]
#[path = "../tests/bin/transport_bench.rs"]
mod tests;
const BUDGET: usize = 128;
enum App {
    Tcp(tcp::App),
    Rdma(rdma::App),
}
impl uring::Application for App {
    fn poll(&mut self, ring: &mut uring::Ring, budget: usize) -> io::Result<uring::Work> {
        match self {
            Self::Tcp(a) => a.poll(ring, budget),
            Self::Rdma(a) => a.poll(ring, budget),
        }
    }
    fn shutdown(&mut self, ring: &mut uring::Ring) -> io::Result<()> {
        match self {
            Self::Tcp(a) => a.shutdown(ring),
            Self::Rdma(a) => a.shutdown(ring),
        }
    }
}
static STOP: AtomicBool = AtomicBool::new(false);
fn invalid(message: &str) -> io::Error {
    io::Error::new(io::ErrorKind::InvalidInput, message)
}
fn timed_out(message: &str) -> io::Error {
    io::Error::new(io::ErrorKind::TimedOut, message)
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Kind {
    Metadata,
    TcpPage,
    Rdma,
}
impl Kind {
    fn bytes(self) -> usize {
        if self == Self::Metadata {
            crate::metadata::Metadata::SIZE
        } else {
            BUFFER_SIZE
        }
    }
}
#[derive(Clone)]
struct Options {
    kind: Kind,
    server: bool,
    address: SocketAddr,
    connections: usize,
    depth: usize,
    warmup: Duration,
    duration: Duration,
    timeout: Duration,
    body: String,
    slab_dir: Option<PathBuf>,
    rail: Option<String>,
}
impl Options {
    fn parse(kind: Kind, args: impl IntoIterator<Item = String>) -> io::Result<Option<Self>> {
        let mut args = args.into_iter();
        let mode = args.next().unwrap_or_default();
        if mode == "--help" || mode.is_empty() {
            println!(
                "{kind:?} benchmark\n  server --listen IP:PORT [--body buffer|file --slab-dir EXT4_DIRECTORY]\n  client --connect IP:PORT [--connections-per-worker N --warmup SECONDS --duration SECONDS]\n  --request-timeout SECONDS (both modes, default 5)\n  RDMA: list-rails; server/client --rail DEVICE:PORT:GID [--depth N]\nTLS uses ephemeral untrusted certificates. Development networks only. Use taskset for physical-core placement. RDMA payloads are plaintext; control is TLS."
            );
            return Ok(None);
        }
        if kind == Kind::Rdma && mode == "list-rails" {
            if args.next().is_some() {
                return Err(invalid("list-rails takes no options"));
            }
            let rails = crate::rdma::discover()?;
            for rail in &rails {
                println!("{} numa={:?}", rail.benchmark_selector(), rail.numa_node);
            }
            if rails.is_empty() {
                return Err(io::Error::new(
                    io::ErrorKind::NotFound,
                    "no supported RDMA rails",
                ));
            }
            return Ok(None);
        }
        let server = match mode.as_str() {
            "server" => true,
            "client" => false,
            _ => return Err(invalid("expected server, client or --help")),
        };
        let mut o = Self {
            kind,
            server,
            address: "127.0.0.1:8080".parse().unwrap(),
            connections: 8,
            depth: 2,
            warmup: Duration::from_secs(3),
            duration: Duration::from_secs(15),
            timeout: Duration::from_secs(5),
            body: "buffer".into(),
            slab_dir: None,
            rail: None,
        };
        while let Some(flag) = args.next() {
            let value = args.next().ok_or_else(|| invalid("missing option value"))?;
            let count = |max| -> io::Result<usize> {
                let n = value
                    .parse()
                    .map_err(|_| invalid("invalid positive integer"))?;
                if !(1..=max).contains(&n) {
                    return Err(invalid("option outside supported range"));
                }
                Ok(n)
            };
            match flag.as_str() {
                "--listen" if server => {
                    o.address = value
                        .parse()
                        .map_err(|_| invalid("invalid listen address"))?
                }
                "--connect" if !server => {
                    o.address = value
                        .parse()
                        .map_err(|_| invalid("invalid connect address"))?
                }
                "--connections-per-worker" => {
                    o.connections = count(if kind == Kind::Rdma { 32 } else { 128 })?
                }
                "--depth" if kind == Kind::Rdma => o.depth = count(16)?,
                "--warmup" if !server => o.warmup = Duration::from_secs(count(3600)? as u64),
                "--duration" if !server => o.duration = Duration::from_secs(count(3600)? as u64),
                "--request-timeout" => o.timeout = Duration::from_secs(count(30)? as u64),
                "--body"
                    if kind == Kind::TcpPage && matches!(value.as_str(), "buffer" | "file") =>
                {
                    o.body = value
                }
                "--slab-dir" if server && kind == Kind::TcpPage => o.slab_dir = Some(value.into()),
                "--rail" if kind == Kind::Rdma => o.rail = Some(value),
                _ => return Err(invalid("unknown option for this mode")),
            }
        }
        if o.address.port() == 0 {
            return Err(invalid("port must be nonzero"));
        }
        if server && o.body == "file" && o.slab_dir.is_none() {
            return Err(invalid("file mode requires --slab-dir"));
        }
        if o.slab_dir.is_some() && o.body != "file" {
            return Err(invalid("--slab-dir requires file mode"));
        }
        if kind == Kind::Rdma
            && (o.rail.is_none()
                || o.warmup + o.duration + o.timeout * 3 + Duration::from_secs(2)
                    >= Duration::from_secs(270))
        {
            return Err(invalid(
                "RDMA needs --rail and setup/warmup/duration/drain below 270 seconds",
            ));
        }
        Ok(Some(o))
    }
}

// Fixed logarithmic histogram: 64 subdivisions per power of two, upper-bound
// quantiles with <1.6% relative rounding. No allocations or shared locks per op.
#[derive(Clone)]
struct Histogram {
    bins: [u64; 4096],
    count: u64,
}
impl Default for Histogram {
    fn default() -> Self {
        Self {
            bins: [0; 4096],
            count: 0,
        }
    }
}
impl Histogram {
    fn index(ns: u64) -> usize {
        let shift = (63 - ns.max(1).leading_zeros()).saturating_sub(6);
        (shift as usize * 64 + (ns >> shift) as usize).min(4095)
    }
    fn record(&mut self, elapsed: Duration) {
        self.bins[Self::index(elapsed.as_nanos().min(u64::MAX as u128) as u64)] += 1;
        self.count += 1;
    }
    fn merge(&mut self, other: &Self) {
        for (a, b) in self.bins.iter_mut().zip(other.bins) {
            *a += b;
        }
        self.count += other.count;
    }
    fn percentile(&self, percent: u64) -> u64 {
        let target = (self.count * percent).div_ceil(100);
        let mut total = 0;
        for (i, n) in self.bins.iter().enumerate() {
            total += n;
            if total >= target && *n > 0 {
                let shift = (i / 64).saturating_sub(1);
                return ((i - shift * 64 + 1) as u64)
                    .checked_shl(shift as u32)
                    .unwrap_or(u64::MAX)
                    .saturating_sub(1);
            }
        }
        0
    }
}
#[derive(Clone, Copy)]
struct Window {
    start: Instant,
    end: Instant,
}
#[derive(Default)]
struct Shared {
    ready: AtomicUsize,
    done: AtomicUsize,
    window: OnceLock<Window>,
    results: Mutex<Vec<(usize, Histogram)>>,
}
struct Measure {
    shared: Arc<Shared>,
    worker: usize,
    warm_until: Instant,
    startup_end: Instant,
    timeout: Duration,
    ready: bool,
    finished: bool,
    histogram: Histogram,
}
impl Measure {
    fn new(o: &Options, shared: Arc<Shared>, worker: usize) -> Self {
        let now = Instant::now();
        Self {
            shared,
            worker,
            warm_until: now + o.warmup,
            startup_end: now + o.warmup + o.timeout * 2,
            timeout: o.timeout,
            ready: false,
            finished: false,
            histogram: Histogram::default(),
        }
    }
    fn admitting(&self, now: Instant) -> bool {
        if let Some(w) = self.shared.window.get() {
            now >= w.start && now < w.end
        } else {
            !self.ready && now < self.warm_until
        }
    }
    fn completion(&mut self, started: Instant, completed: Instant) {
        if self
            .shared
            .window
            .get()
            .is_some_and(|w| started >= w.start && completed <= w.end)
        {
            self.histogram.record(completed.duration_since(started));
        }
    }
    fn progress(&mut self, idle: bool, verified: bool) -> io::Result<uring::Work> {
        let now = Instant::now();
        if !self.ready && now >= self.warm_until && idle && verified {
            self.ready = true;
            self.shared.ready.fetch_add(1, Ordering::Release);
        }
        if !self.ready && now >= self.startup_end {
            return Err(timed_out("not all connections completed warmup"));
        }
        if let Some(w) = self.shared.window.get() {
            if now >= w.end && idle && !self.finished {
                if self.histogram.count == 0 {
                    return Err(invalid("worker measured no complete operations"));
                }
                self.shared
                    .results
                    .lock()
                    .unwrap()
                    .push((self.worker, std::mem::take(&mut self.histogram)));
                self.finished = true;
                self.shared.done.fetch_add(1, Ordering::Release);
            }
            if now >= w.end + self.timeout {
                return Err(timed_out("transport drain timed out"));
            }
            Ok(uring::Work {
                runnable: false,
                deadline: Some(if now < w.start {
                    w.start
                } else if now < w.end {
                    w.end
                } else {
                    w.end + self.timeout
                }),
            })
        } else {
            Ok(uring::Work {
                runnable: false,
                deadline: Some(now + Duration::from_millis(1)),
            })
        }
    }
}
extern "C" fn signal(_: libc::c_int) {
    STOP.store(true, Ordering::Relaxed);
}
fn signals() -> io::Result<()> {
    unsafe {
        let mut action: libc::sigaction = std::mem::zeroed();
        action.sa_sigaction = signal as *const () as usize;
        libc::sigemptyset(&mut action.sa_mask);
        for sig in [libc::SIGINT, libc::SIGTERM] {
            if libc::sigaction(sig, &action, std::ptr::null_mut()) != 0 {
                return Err(io::Error::last_os_error());
            }
        }
    }
    Ok(())
}

pub fn run(kind: Kind) -> io::Result<()> {
    let Some(o) = Options::parse(kind, std::env::args().skip(1))? else {
        return Ok(());
    };
    if cfg!(debug_assertions) {
        return Err(invalid("build benchmarks with --release"));
    }
    signals()?;
    let tls = fixture::tls(o.server)?;
    let allowed = thread::available_parallelism()?.get();
    let count = allowed
        .checked_mul(if o.server || kind == Kind::Metadata {
            1
        } else {
            o.connections * if kind == Kind::Rdma { o.depth } else { 1 }
        })
        .filter(|n| *n <= 65536)
        .ok_or_else(|| invalid("pool provisioning exceeds 65536 buffers"))?;
    let pools = buffers::Pools::new(buffers::Config::new(NonZeroUsize::new(count).unwrap()));
    let shared = Arc::new(Shared::default());
    let app_shared = shared.clone();
    let options = o.clone();
    let rail = if kind == Kind::Rdma {
        Some(
            crate::rdma::discover()?
                .into_iter()
                .find(|r| Some(r.benchmark_selector()) == o.rail)
                .ok_or_else(|| {
                    io::Error::new(
                        io::ErrorKind::NotFound,
                        "requested RDMA rail unavailable; no fallback",
                    )
                })?,
        )
    } else {
        None
    };
    if kind == Kind::Rdma && allowed * o.connections * o.depth * 4 > 65536 {
        return Err(invalid(
            "RDMA control provisioning exceeds production budget",
        ));
    }
    let slab = tcp::slab(&o, allowed)?;
    let group = workers::Workers::start(
        workers::Config {
            shard_count: NonZeroUsize::new(allowed).unwrap(),
        },
        move |placement| {
            let pool = pools.for_worker(placement)?;
            let ring = uring::Ring::new(
                placement,
                pool.clone(),
                uring::Config {
                    fixed_files: 1024,
                    ..Default::default()
                },
            )?;
            // Benchmark fidelity: use the production driver, including arm/recheck and
            // source shutdown ordering. Never replace this with a benchmark CQ spin loop.
            if let Some(rail) = &rail {
                rdma::driver(
                    &options,
                    placement,
                    pool,
                    ring,
                    &tls,
                    rail.clone(),
                    app_shared.clone(),
                )
            } else {
                tcp::driver(
                    &options,
                    placement,
                    pool,
                    ring,
                    &tls,
                    slab.as_ref(),
                    app_shared.clone(),
                )
            }
        },
    )?;
    let n = group.placements().len();
    for p in group.placements() {
        eprintln!(
            "worker={} cpu={} numa={}",
            p.worker_id().0,
            p.cpu_id().0,
            p.numa_node_id().0
        );
    }
    eprintln!(
        "READY kind={kind:?} mode={} address={} workers={n} connections_per_worker={} body={} bytes={} rail={:?} depth={} pool_buffers_per_node={count}",
        if o.server { "server" } else { "client" },
        o.address,
        o.connections,
        o.body,
        kind.bytes(),
        o.rail,
        if kind == Kind::Rdma { o.depth } else { 1 }
    );
    let stop = group.stop_handle();
    let finished = Arc::new(AtomicBool::new(false));
    let done = finished.clone();
    let monitor_shared = shared.clone();
    let monitor_options = o.clone();
    let monitor = thread::spawn(move || {
        let deadline = Instant::now() + monitor_options.warmup + monitor_options.timeout * 2;
        while !done.load(Ordering::Acquire) {
            if STOP.load(Ordering::Relaxed) {
                stop.request_stop();
                break;
            }
            if !monitor_options.server {
                if monitor_shared.window.get().is_none()
                    && monitor_shared.ready.load(Ordering::Acquire) == n
                {
                    let start = Instant::now() + Duration::from_millis(50);
                    monitor_shared
                        .window
                        .set(Window {
                            start,
                            end: start + monitor_options.duration,
                        })
                        .ok();
                }
                if monitor_shared.done.load(Ordering::Acquire) == n
                    || (monitor_shared.window.get().is_none() && Instant::now() >= deadline)
                {
                    stop.request_stop();
                    break;
                }
            }
            thread::park_timeout(Duration::from_millis(1));
        }
    });
    let result = group.join();
    finished.store(true, Ordering::Release);
    monitor.thread().unpark();
    monitor
        .join()
        .map_err(|_| io::Error::other("monitor panicked"))?;
    eprintln!("TLS {:?}", crate::tls::global_counters());
    if let Err(e) = result {
        eprintln!("FAILED errors=1: {e}");
        return Err(e);
    }
    if o.server {
        return Ok(());
    }
    if STOP.load(Ordering::Relaxed) {
        return Err(io::ErrorKind::Interrupted.into());
    }
    let rows = shared.results.lock().unwrap();
    if rows.len() != n {
        return Err(timed_out("missing worker results"));
    }
    let mut h = Histogram::default();
    for (worker, row) in rows.iter() {
        println!("worker={worker} completed={}", row.count);
        h.merge(row);
    }
    let secs = o.duration.as_secs_f64();
    let bytes = h.count as f64 * kind.bytes() as f64;
    println!(
        "LATENCY p50_ms={:.6} p95_ms={:.6} p99_ms={:.6} histogram_relative_error=0.016",
        h.percentile(50) as f64 / 1e6,
        h.percentile(95) as f64 / 1e6,
        h.percentile(99) as f64 / 1e6
    );
    println!(
        "RESULT kind={kind:?} workers={n} connections_per_worker={} body={} payload_bytes={} seconds={secs} completed={} ops_per_second={:.3} gib_per_second={:.3} gbit_per_second={:.3} errors=0",
        o.connections,
        o.body,
        kind.bytes(),
        h.count,
        h.count as f64 / secs,
        bytes / secs / 1073741824.0,
        bytes * 8.0 / secs / 1e9
    );
    Ok(())
}
