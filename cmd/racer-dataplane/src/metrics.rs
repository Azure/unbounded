// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Worker-private counters, active outbound peer snapshots and config epoch.
use std::{
    cell::Cell,
    fmt::Write as _,
    io::{self, Read, Write},
    net::{SocketAddr, TcpListener, TcpStream},
    rc::Rc,
    sync::{
        Arc, Mutex, OnceLock,
        atomic::{AtomicBool, AtomicU64, Ordering},
    },
    thread::{self, JoinHandle},
    time::{Duration, Instant},
};

const ERROR_BASE: usize = 21;
const REASONS: usize = 15;
const ABORT_BASE: usize = ERROR_BASE + 2 * REASONS;
const PRESSURE_BASE: usize = ABORT_BASE + 2 * REASONS;
const PRESSURES: usize = 4;
const RESOURCE_BASE: usize = PRESSURE_BASE + 2 * 2 * PRESSURES;
const RESOURCE_SITES: [&str; 8] = [
    "network_flight",
    "metadata_admission",
    "upstream_admission",
    "payload_admission",
    "materialize_read",
    "materialize_buffer",
    "checksum_queue",
    "receive_buffer",
];
const DISK_CACHE_EVICTIONS: usize = RESOURCE_BASE + RESOURCE_SITES.len();
const COUNT: usize = DISK_CACHE_EVICTIONS + 1;
pub(crate) const INTERVAL: Duration = Duration::from_millis(250);

/// Terminal local wait site, not the history of the fault's shared retry budget.
/// Count at exhaustion before fanout, never when a joiner or peer consumes Busy.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(crate) enum ResourceWaitSite {
    NetworkFlight,
    MetadataAdmission,
    UpstreamAdmission,
    PayloadAdmission,
    MaterializeRead,
    MaterializeBuffer,
    ChecksumQueue,
    ReceiveBuffer,
}

/// Finite handler outcomes, independent of targets, peers and error strings.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(crate) enum HttpErrorReason {
    OwnerUnavailable,
    Busy,
    Unavailable,
    Protocol,
    Service,
    Deadline,
    Cancelled,
    NotFound,
    Gone,
    Precondition,
    BadRequest,
    UriTooLong,
    Range,
    Unprocessable,
    Other,
}
const HTTP_ERRORS: [(&str, &str); REASONS] = [
    ("owner_unavailable", "503"),
    ("busy", "503"),
    ("unavailable", "503"),
    ("protocol", "502"),
    ("service", "502"),
    ("deadline", "504"),
    ("cancelled", "502"),
    ("not_found", "404"),
    ("gone", "410"),
    ("precondition", "412"),
    ("bad_request", "400"),
    ("uri_too_long", "414"),
    ("range", "416"),
    ("unprocessable", "422"),
    ("other", "other"),
];
impl HttpErrorReason {
    pub(crate) fn status(status: u16) -> Self {
        match status {
            400 => Self::BadRequest,
            404 => Self::NotFound,
            410 => Self::Gone,
            412 => Self::Precondition,
            414 => Self::UriTooLong,
            416 => Self::Range,
            422 => Self::Unprocessable,
            502 => Self::Service,
            503 => Self::Busy,
            504 => Self::Deadline,
            _ => Self::Other,
        }
    }
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(crate) enum HttpPressure {
    Admission,
    LocalPressure,
    BreakerRejected,
    WouldBlock,
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(crate) struct HttpFailure {
    pub reason: HttpErrorReason,
    pub pressure: Option<HttpPressure>,
}

/// Only active volume generations contribute; replaces the whole worker view.
#[derive(Debug, PartialEq, Eq)]
pub(crate) struct PeerState {
    pub volume: String,
    pub peer: String,
    pub http: crate::breaker::Status,
    pub rdma: crate::breaker::Status,
    pub prefer_rdma: bool,
}

#[derive(Clone, Copy)]
pub enum Traffic {
    ClientHttp,
    PeerHttp,
    PeerRdma,
}
#[derive(Clone, Copy)]
pub enum Kind {
    Metadata,
    Page,
}
#[derive(Clone, Copy)]
pub enum Outcome {
    /// Resident inline metadata lookup; payloads never hit a completed buffer pool.
    MetadataHit,
    DiskHit,
    Miss,
    Coalesced,
}
#[derive(Clone, Copy)]
pub enum Upstream {
    BackendHttp,
    PeerHttp,
    PeerRdma,
}

// Match buffers' isolation convention, including adjacent-line prefetch on x86.
// Rc/Arc control words live before, not inside, these aligned payloads.
#[repr(align(128))]
struct Private {
    values: [Cell<u64>; COUNT],
    dirty: Cell<bool>,
}
#[repr(align(128))]
pub struct Snapshot([AtomicU64; COUNT], Mutex<Arc<[PeerState]>>);

/// Clone only during component setup. This handle cannot cross worker threads.
#[derive(Clone)]
pub struct Local {
    private: Rc<Private>,
    snapshot: Arc<Snapshot>,
}
impl Default for Local {
    fn default() -> Self {
        Self {
            private: Rc::new(Private {
                values: std::array::from_fn(|_| Cell::new(0)),
                dirty: Cell::new(false),
            }),
            snapshot: Arc::new(Snapshot(
                std::array::from_fn(|_| AtomicU64::new(0)),
                Mutex::new(Arc::from([])),
            )),
        }
    }
}
impl Local {
    /// A scrape only holds this lock to clone the immutable snapshot. Workers
    /// never wait for it; contention leaves the previous view until the next tick.
    pub(crate) fn publish_peers(&self, peers: Vec<PeerState>) {
        let peers = Arc::from(peers);
        if let Ok(mut published) = self.snapshot.1.try_lock() {
            let old = std::mem::replace(&mut *published, peers);
            drop(published);
            drop(old);
        }
    }
    #[cfg(test)]
    pub(crate) fn values(&self) -> [u64; COUNT] {
        std::array::from_fn(|i| self.private.values[i].get())
    }
    #[inline]
    fn add(&self, index: usize, amount: u64) {
        let counter = &self.private.values[index];
        counter.set(counter.get().wrapping_add(amount));
        self.private.dirty.set(true);
    }
    #[inline]
    pub fn request(&self, traffic: Traffic) {
        self.add(traffic as usize, 1);
    }
    #[inline]
    pub fn bytes(&self, traffic: Traffic, amount: u64) {
        self.add(3 + traffic as usize, amount);
    }
    #[inline]
    pub fn lookup(&self, kind: Kind, outcome: Outcome) {
        self.add(6 + kind as usize * 4 + outcome as usize, 1);
    }
    #[inline]
    pub fn upstream(&self, upstream: Upstream, kind: Kind) {
        self.add(14 + upstream as usize * 2 + kind as usize, 1);
    }
    pub(crate) fn storage_quarantine(&self) {
        self.add(20, 1);
    }
    pub(crate) fn disk_cache_evictions(&self, count: u64) {
        if count != 0 {
            self.add(DISK_CACHE_EVICTIONS, count);
        }
    }
    pub(crate) fn resource_exhaustion(&self, site: ResourceWaitSite) {
        self.add(RESOURCE_BASE + site as usize, 1);
    }
    pub(crate) fn http_failure(&self, peer: bool, abort: bool, failure: HttpFailure) {
        let source = usize::from(peer);
        let base = if abort { ABORT_BASE } else { ERROR_BASE };
        self.add(base + source * REASONS + failure.reason as usize, 1);
        if let Some(pressure) = failure.pressure {
            self.add(
                PRESSURE_BASE + (source * 2 + usize::from(abort)) * PRESSURES + pressure as usize,
                1,
            );
        }
    }
    pub fn publish(&self) {
        if self.private.dirty.replace(false) {
            for (value, published) in self.private.values.iter().zip(&self.snapshot.0) {
                published.store(value.get(), Ordering::Relaxed);
            }
        }
    }
    /// Called by the driver, never per event. No wakeups while clean, and no
    /// per-sleep flush that would degenerate into per-request publication.
    pub(crate) fn poll(&self, deadline: &mut Option<Instant>) -> crate::uring::Work {
        if !self.private.dirty.get() {
            return crate::uring::Work::default();
        }
        let now = crate::environment::now();
        let end = *deadline.get_or_insert(now + INTERVAL);
        if now >= end {
            self.publish();
            *deadline = None;
            crate::uring::Work::default()
        } else {
            crate::uring::Work {
                runnable: false,
                deadline: Some(end),
            }
        }
    }
}

#[cfg(test)]
include!(concat!(env!("CARGO_MANIFEST_DIR"), "/tests/metrics.rs"));

/// Fixed worker slots. Registration happens once, inside the pinned factory.
pub struct Registry {
    lifecycle: Option<Arc<crate::lifecycle::Lifecycle>>,
    workers: Vec<OnceLock<Arc<Snapshot>>>,
    updates: Arc<crate::control::Updates>,
}
impl Registry {
    pub fn new(workers: usize, updates: Arc<crate::control::Updates>) -> Self {
        Self {
            lifecycle: None,
            workers: (0..workers).map(|_| OnceLock::new()).collect(),
            updates,
        }
    }
    pub fn with_lifecycle(mut self, lifecycle: Arc<crate::lifecycle::Lifecycle>) -> Self {
        self.lifecycle = Some(lifecycle);
        self
    }
    fn status(&self) -> serde_json::Value {
        let mut status = self.updates.status();
        if let Some(life) = &self.lifecycle {
            let healthy = life.healthy();
            status["ready"] = (status["ready"] == true && healthy).into();
            status["workerHealthy"] = healthy.into();
            status["draining"] = life.draining().into();
            if !healthy && let Some(volumes) = status["volumes"].as_array_mut() {
                for volume in volumes {
                    volume["ready"] = false.into();
                }
            }
        }
        status
    }
    pub fn register(&self, worker: usize, local: &Local) {
        assert!(
            self.workers[worker].set(local.snapshot.clone()).is_ok(),
            "worker registered twice"
        );
    }
    pub fn render(&self) -> String {
        let mut totals = [0u64; COUNT];
        for snapshot in self.workers.iter().filter_map(OnceLock::get) {
            for (sum, value) in totals.iter_mut().zip(&snapshot.0) {
                *sum = sum.wrapping_add(value.load(Ordering::Relaxed));
            }
        }
        let mut out = String::with_capacity(4096);
        writeln!(out, "# HELP racer_dataplane_config_epoch Last configuration epoch activated by all workers; zero means unknown.\n# TYPE racer_dataplane_config_epoch gauge\nracer_dataplane_config_epoch {}", self.updates.applied_epoch()).unwrap();
        let mut family = |name: &str, help: &str| {
            writeln!(
                out,
                "# HELP racer_dataplane_{name} {help}\n# TYPE racer_dataplane_{name} counter"
            )
            .unwrap();
        };
        family(
            "requests_total",
            "Incoming data requests, excluding negotiation.",
        );
        for (i, (source, transport)) in TRAFFIC.iter().enumerate() {
            writeln!(out, "racer_dataplane_requests_total{{source=\"{source}\",transport=\"{transport}\"}} {}", totals[i]).unwrap();
        }
        writeln!(out, "# HELP racer_dataplane_response_bytes_total Payload bytes sent by HTTP or acknowledged by RDMA.\n# TYPE racer_dataplane_response_bytes_total counter").unwrap();
        for (i, (source, transport)) in TRAFFIC.iter().enumerate() {
            writeln!(out, "racer_dataplane_response_bytes_total{{source=\"{source}\",transport=\"{transport}\"}} {}", totals[3+i]).unwrap();
        }
        writeln!(out, "# HELP racer_dataplane_cache_lookups_total Once-only metadata/page lookup classifications.\n# TYPE racer_dataplane_cache_lookups_total counter").unwrap();
        for (kind, name) in ["metadata", "page"].iter().enumerate() {
            for (result, outcome) in ["memory_hit", "disk_hit", "miss", "coalesced"]
                .iter()
                .enumerate()
            {
                // Only inline metadata has a userspace memory hit. Payload file
                // hits use the kernel page cache; metadata never needs a disk read.
                if (kind == 1 && result == 0) || (kind == 0 && result == 1) {
                    continue;
                }
                writeln!(out, "racer_dataplane_cache_lookups_total{{kind=\"{name}\",result=\"{outcome}\"}} {}", totals[6+kind*4+result]).unwrap();
            }
        }
        writeln!(out, "# HELP racer_dataplane_upstream_requests_total Admitted outbound fetch attempts, including retries.\n# TYPE racer_dataplane_upstream_requests_total counter").unwrap();
        for (i, (destination, transport)) in
            [("backend", "http"), ("peer", "http"), ("peer", "rdma")]
                .iter()
                .enumerate()
        {
            for (kind, name) in ["metadata", "page"].iter().enumerate() {
                writeln!(out, "racer_dataplane_upstream_requests_total{{destination=\"{destination}\",transport=\"{transport}\",kind=\"{name}\"}} {}", totals[14+i*2+kind]).unwrap();
            }
        }
        writeln!(out, "# HELP racer_dataplane_disk_cache_evictions_total Payload items evicted to make room for new cache fills, counted when removed even if the fill later fails. Excludes metadata, replacement, invalidation and corruption cleanup.\n# TYPE racer_dataplane_disk_cache_evictions_total counter\nracer_dataplane_disk_cache_evictions_total {}", totals[DISK_CACHE_EVICTIONS]).unwrap();
        crate::http_auth::replay::render(&mut out);
        writeln!(out, "# HELP racer_dataplane_cache_resource_exhaustions_total Locally originated terminal retry exhaustion by final wait site; budget is shared across sites. Excludes joiner/peer propagation, immediate rejection, cancellation and deadlines.\n# TYPE racer_dataplane_cache_resource_exhaustions_total counter").unwrap();
        for (site, name) in RESOURCE_SITES.iter().enumerate() {
            writeln!(
                out,
                "racer_dataplane_cache_resource_exhaustions_total{{site=\"{name}\"}} {}",
                totals[RESOURCE_BASE + site]
            )
            .unwrap();
        }
        writeln!(out, "# HELP racer_dataplane_http_error_responses_total Handler error responses whose headers finished sending; excludes management and HTTP parser errors.\n# TYPE racer_dataplane_http_error_responses_total counter").unwrap();
        for (source, name) in ["client", "peer"].iter().enumerate() {
            for (reason, (label, status)) in HTTP_ERRORS.iter().enumerate() {
                writeln!(out, "racer_dataplane_http_error_responses_total{{source=\"{name}\",status=\"{status}\",reason=\"{label}\"}} {}", totals[ERROR_BASE + source * REASONS + reason]).unwrap();
            }
        }
        writeln!(out, "# HELP racer_dataplane_http_stream_aborts_total Handler failures after response headers finished sending; excludes transport teardown outside handler polling.\n# TYPE racer_dataplane_http_stream_aborts_total counter").unwrap();
        for (source, name) in ["client", "peer"].iter().enumerate() {
            for (reason, (label, _)) in HTTP_ERRORS.iter().enumerate() {
                writeln!(out, "racer_dataplane_http_stream_aborts_total{{source=\"{name}\",reason=\"{label}\"}} {}", totals[ABORT_BASE + source * REASONS + reason]).unwrap();
            }
        }
        writeln!(out, "# HELP racer_dataplane_http_pressure_failures_total Subset of emitted handler errors or stream aborts with typed resource pressure evidence; admission includes retry exhaustion and storage admission.\n# TYPE racer_dataplane_http_pressure_failures_total counter").unwrap();
        for (source, name) in ["client", "peer"].iter().enumerate() {
            for (event, event_name) in ["error_response", "stream_abort"].iter().enumerate() {
                for (cause, label) in [
                    "admission",
                    "local_pressure",
                    "breaker_rejected",
                    "would_block",
                ]
                .iter()
                .enumerate()
                {
                    writeln!(out, "racer_dataplane_http_pressure_failures_total{{source=\"{name}\",event=\"{event_name}\",cause=\"{label}\"}} {}", totals[PRESSURE_BASE + (source * 2 + event) * PRESSURES + cause]).unwrap();
                }
            }
        }
        writeln!(out, "# HELP racer_dataplane_storage_quarantines_total Shards permanently quarantined after ambiguous storage IO; restart required.\n# TYPE racer_dataplane_storage_quarantines_total counter\nracer_dataplane_storage_quarantines_total {}", totals[20]).unwrap();
        let peers: Vec<_> = self
            .workers
            .iter()
            .enumerate()
            .filter_map(|(worker, slot)| Some((worker, slot.get()?.1.lock().unwrap().clone())))
            .collect();
        writeln!(out, "# HELP racer_dataplane_peer_circuit_breaker_state Active outbound peer transport breaker state (one-hot); half_open means a probe is in flight.\n# TYPE racer_dataplane_peer_circuit_breaker_state gauge").unwrap();
        for (worker, peers) in &peers {
            for peer in peers.iter() {
                let labels = format!(
                    "worker=\"{worker}\",volume=\"{}\",peer=\"{}\"",
                    label(&peer.volume),
                    label(&peer.peer)
                );
                for (transport, status) in [("http", peer.http), ("rdma", peer.rdma)] {
                    for (state, value) in [
                        ("closed", crate::breaker::Status::Closed),
                        ("open", crate::breaker::Status::Open),
                        ("half_open", crate::breaker::Status::HalfOpen),
                    ] {
                        writeln!(out, "racer_dataplane_peer_circuit_breaker_state{{{labels},transport=\"{transport}\",state=\"{state}\"}} {}", u8::from(status == value)).unwrap();
                    }
                }
            }
        }
        writeln!(out, "# HELP racer_dataplane_peer_transport Preferred transport for new outbound peer requests (one-hot), not guaranteed availability.\n# TYPE racer_dataplane_peer_transport gauge").unwrap();
        for (worker, peers) in &peers {
            for peer in peers.iter() {
                let labels = format!(
                    "worker=\"{worker}\",volume=\"{}\",peer=\"{}\"",
                    label(&peer.volume),
                    label(&peer.peer)
                );
                for (transport, preferred) in
                    [("http", !peer.prefer_rdma), ("rdma", peer.prefer_rdma)]
                {
                    writeln!(
                        out,
                        "racer_dataplane_peer_transport{{{labels},transport=\"{transport}\"}} {}",
                        u8::from(preferred)
                    )
                    .unwrap();
                }
            }
        }
        out
    }
}
const TRAFFIC: [(&str, &str); 3] = [("client", "http"), ("peer", "http"), ("peer", "rdma")];

fn label(value: &str) -> String {
    value
        .replace('\\', "\\\\")
        .replace('"', "\\\"")
        .replace('\n', "\\n")
}

/// Single nonblocking exporter thread; at most 32 clients, 8 KiB headers and a
/// two-second absolute exchange deadline. It never waits on an I/O worker.
pub struct Exporter {
    address: SocketAddr,
    stop: Arc<AtomicBool>,
    thread: Option<JoinHandle<()>>,
}
impl Exporter {
    pub fn start(address: SocketAddr, registry: Arc<Registry>) -> io::Result<Self> {
        let listener = TcpListener::bind(address)?;
        let address = listener.local_addr()?;
        listener.set_nonblocking(true)?;
        let stop = Arc::new(AtomicBool::new(false));
        let stopping = stop.clone();
        let thread = thread::Builder::new()
            .name("racer-metrics".into())
            .spawn(move || {
                let mut clients = Vec::new();
                while !stopping.load(Ordering::Relaxed) {
                    for _ in clients.len()..32 {
                        match listener.accept() {
                            Ok((socket, _)) => {
                                if socket.set_nonblocking(true).is_ok() {
                                    clients.push(Client {
                                        socket,
                                        input: Vec::new(),
                                        output: None,
                                        sent: 0,
                                        end: Instant::now() + Duration::from_secs(2),
                                    });
                                }
                            }
                            Err(e) if e.kind() == io::ErrorKind::Interrupted => continue,
                            Err(_) => break,
                        }
                    }
                    clients.retain_mut(|client| client.poll(&registry).unwrap_or(false));
                    thread::park_timeout(Duration::from_millis(10));
                }
            })?;
        Ok(Self {
            address,
            stop,
            thread: Some(thread),
        })
    }
    pub fn address(&self) -> SocketAddr {
        self.address
    }
}
impl Drop for Exporter {
    fn drop(&mut self) {
        self.stop.store(true, Ordering::Relaxed);
        if let Some(thread) = self.thread.take() {
            thread.thread().unpark();
            let _ = thread.join();
        }
    }
}
struct Client {
    socket: TcpStream,
    input: Vec<u8>,
    output: Option<Vec<u8>>,
    sent: usize,
    end: Instant,
}
impl Client {
    fn poll(&mut self, registry: &Registry) -> io::Result<bool> {
        if Instant::now() >= self.end {
            return Ok(false);
        }
        if self.output.is_none() {
            let mut bytes = [0; 1024];
            match self.socket.read(&mut bytes) {
                Ok(0) => return Ok(false),
                Ok(n) => self.input.extend_from_slice(&bytes[..n]),
                Err(e) if e.kind() == io::ErrorKind::WouldBlock => return Ok(true),
                Err(e) => return Err(e),
            }
            if self.input.len() > 8192 {
                return Ok(false);
            }
            if !self.input.windows(4).any(|b| b == b"\r\n\r\n") {
                return Ok(true);
            }
            let line = self.input.split(|b| *b == b'\n').next().unwrap_or_default();
            let (status, body) =
                if line == b"GET /metrics HTTP/1.1\r" || line == b"GET /metrics HTTP/1.0\r" {
                    ("200 OK", registry.render())
                } else if line == b"GET /readyz HTTP/1.1\r" {
                    let status = registry.status();
                    (
                        if status["ready"] == true {
                            "200 OK"
                        } else {
                            "503 Service Unavailable"
                        },
                        status.to_string(),
                    )
                } else if line == b"GET /status HTTP/1.1\r" {
                    ("200 OK", registry.status().to_string())
                } else if line == b"GET /livez HTTP/1.1\r" || line == b"GET /startupz HTTP/1.1\r" {
                    let healthy = registry
                        .lifecycle
                        .as_ref()
                        .is_some_and(|life| life.healthy());
                    if healthy {
                        ("200 OK", "ok\n".into())
                    } else {
                        ("503 Service Unavailable", "workers unavailable\n".into())
                    }
                } else {
                    ("404 Not Found", String::new())
                };
            self.output = Some(format!("HTTP/1.1 {status}\r\nContent-Type: text/plain; version=0.0.4; charset=utf-8\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}", body.len()).into_bytes());
        }
        let output = self.output.as_ref().unwrap();
        match self.socket.write(&output[self.sent..]) {
            Ok(0) => return Ok(false),
            Ok(n) => self.sent += n,
            Err(e) if e.kind() == io::ErrorKind::WouldBlock => {}
            Err(e) => return Err(e),
        }
        Ok(self.sent < output.len())
    }
}
