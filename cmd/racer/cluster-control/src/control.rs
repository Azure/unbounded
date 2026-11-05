//! Runtime orchestration and the public handle.

use crate::{
    Config, DiscoveredEndpoints, Error, annotations,
    entropy::{Entropy, OsEntropy},
    kube::{Kube, KubeConfig, paths, str_at},
    limits,
    pki::DynamicClientVerifier,
    rt::{self, Executor, Listener, Notifier, Signal},
    rules::rails::{self, Report},
    seat, server,
    shared::{Serving, Shared},
};
use racer_io::exec::Next;
use serde_json::json;
use std::{
    cell::RefCell,
    net::SocketAddr,
    num::NonZeroU32,
    rc::Rc,
    sync::{
        Arc, Condvar, Mutex,
        atomic::{AtomicBool, AtomicU64, Ordering},
    },
    thread::JoinHandle,
    time::{Duration, Instant},
};

/// Result of [`Handle::bootstrap`].
#[derive(Clone, Debug)]
pub struct Bootstrap {
    pub cluster: racer_control_wire::ClusterId,
    pub block_devices: Option<String>,
}

/// Role of this Pod.
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub enum Role {
    #[default]
    Member,
    Seat(usize),
    Primary(usize),
}

/// Point-in-time runtime state for logs and metrics.
#[derive(Clone, Debug, Default)]
pub struct Diagnostics {
    pub role: Role,
    pub sequence: u64,
    pub ready: bool,
    pub tasks: usize,
    pub last_error: Option<Error>,
}

/// State shared with other threads.
struct Outer {
    bootstrap: Mutex<Option<Result<Bootstrap, Error>>>,
    bootstrapped: Condvar,
    report: Mutex<Option<String>>,
    report_generation: AtomicU64,
    stop: AtomicBool,
    diagnostics: Mutex<Diagnostics>,
}

/// Owner of the control thread. Dropping it stops and joins the thread.
pub struct Handle {
    outer: Arc<Outer>,
    endpoints: DiscoveredEndpoints,
    notifier: Notifier,
    thread: Option<JoinHandle<()>>,
}

/// Most tasks on the control thread: connections plus fixed tasks.
const MAX_TASKS: usize = limits::MAX_CONTROL_CONNECTIONS + limits::MAX_WEBHOOK_CONNECTIONS + 64;

/// Bounded wait for the thread to stop. If it is exceeded the thread is
/// detached; it still exits on its own once its tasks next yield, because the
/// stop flag stays set and the run loop's own deadline is shorter.
const JOIN_TIMEOUT: Duration = Duration::from_secs(10);

/// Bounded wait for setup (address resolution, binds) on the control thread.
const START_TIMEOUT: Duration = Duration::from_secs(30);

/// Start the control thread. Listeners are bound before this returns.
pub fn start(config: Config) -> Result<Handle, Error> {
    config.validate()?;
    let outer = Outer::new();
    let (tx, rx) = std::sync::mpsc::sync_channel(1);
    let o = outer.clone();
    let thread = std::thread::Builder::new()
        .name("racer-control".into())
        .spawn(move || run(config, o, tx))
        .map_err(|_| Error::Io)?;
    let (notifier, endpoints) = match rx.recv_timeout(START_TIMEOUT) {
        Ok(r) => r?,
        Err(e) => {
            // The thread sees the flag, or fails to hand over, and exits.
            outer.stop.store(true, Ordering::SeqCst);
            drop(rx);
            return Err(match e {
                std::sync::mpsc::RecvTimeoutError::Timeout => Error::Timeout,
                std::sync::mpsc::RecvTimeoutError::Disconnected => Error::Stopped,
            });
        }
    };
    Ok(Handle {
        outer,
        endpoints,
        notifier,
        thread: Some(thread),
    })
}

/// The control thread: one host [`Authority`] run until it finishes.
fn run(
    config: Config,
    outer: Arc<Outer>,
    ready: std::sync::mpsc::SyncSender<Result<(Notifier, DiscoveredEndpoints), Error>>,
) {
    let executor = match Executor::new(MAX_TASKS) {
        Ok(e) => e,
        Err(_) => {
            let _ = ready.send(Err(Error::Io));
            return;
        }
    };
    let mut authority = match Authority::build(config, executor, Box::new(OsEntropy), outer.clone())
    {
        Ok(a) => a,
        Err(e) => {
            let _ = ready.send(Err(e));
            return;
        }
    };
    let handover = (authority.executor.notifier(), authority.endpoints());
    // Nobody waits any more after a start timeout: exit without serving.
    if ready.send(Ok(handover)).is_err() || outer.stop.load(Ordering::SeqCst) {
        return;
    }
    loop {
        let next = authority.turn();
        if matches!(next, Next::Done) || authority.executor.park(next).is_err() {
            return;
        }
    }
}

/// One Pod's control plane role: seat lottery, primary, control API and
/// webhook servers, and its Kubernetes API clients, on one reactor.
///
/// [`start`] runs one on a dedicated thread over `io_uring`. A deterministic
/// simulation builds one with [`Authority::simulated`] and steps it with
/// [`Authority::turn`] on its own thread, together with other authorities
/// and a fake API server, moving virtual time itself. Not `Send`.
pub struct Authority {
    executor: Executor,
    ctl: Ctl,
}

/// Run state checked after every turn.
struct Ctl {
    shared: Rc<Shared>,
    outer: Arc<Outer>,
    endpoints: DiscoveredEndpoints,
    report_signal: Signal,
    seen_report: u64,
    stop_at: Option<Instant>,
    done: bool,
}

impl Authority {
    /// An authority on the simulated driver. `os` is the simulated network
    /// and disk shared by every node of the world (the Kubernetes CA and
    /// token files are read from it); `clock` is this Pod's clock, which
    /// may run at its own rate. Randomness for jitter and nonces is seeded
    /// from `seed`. Listeners in `config` must use fixed ports.
    pub fn simulated(
        config: Config,
        os: racer_io::sim::SimOs,
        clock: &racer_io::ctx::VirtualClock,
        seed: u64,
    ) -> Result<Self, Error> {
        config.validate()?;
        let executor = Executor::simulated(os, clock, seed, MAX_TASKS).map_err(|_| Error::Io)?;
        let rng = Box::new(racer_core::time::SplitMix64::new(seed));
        Self::build(config, executor, rng, Outer::new())
    }

    /// Bind listeners and spawn every task on `executor`.
    fn build(
        config: Config,
        executor: Executor,
        mut rng: Box<dyn Entropy>,
        outer: Arc<Outer>,
    ) -> Result<Self, Error> {
        let endpoints: DiscoveredEndpoints = Default::default();
        let report_signal = Signal::default();
        let shared = executor.enter(|| -> Result<Rc<Shared>, Error> {
            let kube = KubeConfig {
                server: config.api_server.clone(),
                ca: config.api_ca.clone(),
                token: config.api_token.clone(),
            };
            let lease_api =
                Rc::new(Kube::new(kube.clone()).map_err(|_| Error::InvalidConfiguration)?);
            let api = Rc::new(Kube::new(kube).map_err(|_| Error::InvalidConfiguration)?);
            let control = Listener::bind(config.control_listen).map_err(|_| Error::Io)?;
            let webhook = Listener::bind(config.webhook_listen).map_err(|_| Error::Io)?;
            // Port 0 binds an ephemeral port; advertise the real one.
            let mut config = config;
            config.control_listen = control.local_addr().map_err(|_| Error::Io)?;
            config.webhook_listen = webhook.local_addr().map_err(|_| Error::Io)?;
            // Serve the webhook on the other Pod IP family on the same port, so
            // either family of the racer-webhook Service can reach it.
            let webhook_alternate = config
                .webhook_alternate
                .map(|ip| Listener::bind(SocketAddr::new(ip, config.webhook_listen.port())))
                .transpose()
                .map_err(|_| Error::Io)?;
            let mut nonce = [0u8; 8];
            rng.fill(&mut nonce).map_err(|_| Error::Io)?;
            let nonce: String = nonce.iter().map(|b| format!("{b:02x}")).collect();
            let shared = Rc::new(Shared {
                identity: format!("{}/{nonce}", config.pod_name),
                nonce,
                config,
                cluster: RefCell::new(String::new()),
                lease_api,
                api,
                seat: RefCell::new(None),
                issuer: RefCell::new(None),
                serving: RefCell::new(Serving::default()),
                changed: Signal::default(),
                primary_lease: Signal::default(),
                verifier: DynamicClientVerifier::new(),
                seat_holders: RefCell::new(Default::default()),
                is_primary: Default::default(),
                term: Default::default(),
                primary: RefCell::new(Default::default()),
                stopping: Default::default(),
                releasing: Default::default(),
                last_error: Default::default(),
                rng: RefCell::new(rng),
                authz: Default::default(),
                auth_budget: Default::default(),
            });
            let primary_changed = Rc::new(RefCell::new((String::new(), None)));
            rt::spawn(bootstrap(shared.clone(), outer.clone()))?;
            rt::spawn(seat::discovery(
                shared.clone(),
                endpoints.clone(),
                primary_changed.clone(),
            ))?;
            rt::spawn(seat::lottery(shared.clone(), primary_changed))?;
            rt::spawn(server::Server::new(shared.clone()).run(control))?;
            // One budget across both Pod IP families.
            server::spawn_webhooks(&shared, std::iter::once(webhook).chain(webhook_alternate))?;
            rt::spawn(reporter(
                shared.clone(),
                outer.clone(),
                report_signal.clone(),
            ))?;
            Ok(shared)
        })?;
        Ok(Self {
            executor,
            ctl: Ctl {
                shared,
                outer,
                endpoints,
                report_signal,
                seen_report: 0,
                stop_at: None,
                done: false,
            },
        })
    }

    /// One bounded step: reap completions, poll ready tasks, and apply
    /// reports and stop requests. Returns what the authority needs next:
    /// `Ready` to be turned again, `WaitUntil(t)` on its own clock, `Idle`
    /// until a completion or wake, or `Done` once it stopped. Never blocks.
    pub fn turn(&mut self) -> Next {
        if self.ctl.done {
            return Next::Done;
        }
        if self.executor.turn().is_err() {
            self.ctl.done = true;
            return Next::Done;
        }
        let ctl = &mut self.ctl;
        if self.executor.enter(|| ctl.after_turn()) {
            ctl.done = true;
            return Next::Done;
        }
        self.executor.next()
    }

    /// What [`Self::turn`] would report, without doing work.
    pub fn next(&self) -> Next {
        if self.ctl.done {
            return Next::Done;
        }
        self.executor.next()
    }

    /// Ask the authority to stop. Held Leases are released within a bounded
    /// grace period while it is turned; then [`Self::turn`] returns `Done`.
    pub fn stop(&mut self) {
        self.ctl.outer.stop.store(true, Ordering::SeqCst);
        self.executor.notifier().notify();
    }

    /// The bootstrap result, once known.
    pub fn bootstrap(&self) -> Option<Result<Bootstrap, Error>> {
        self.ctl
            .outer
            .bootstrap
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .clone()
    }

    /// Ready seat endpoints, updated from the primary Lease.
    pub fn endpoints(&self) -> DiscoveredEndpoints {
        self.ctl.endpoints.clone()
    }

    /// Report shares and RDMA NICs in this Pod's report annotation.
    pub fn report(
        &mut self,
        shares: u32,
        rdma_nics: Vec<racer_control_wire::RailMapping>,
    ) -> Result<(), Error> {
        self.ctl.outer.set_report(shares, rdma_nics)?;
        self.executor.notifier().notify();
        Ok(())
    }

    /// Point-in-time state, as of this call.
    pub fn diagnostics(&self) -> Diagnostics {
        self.executor
            .enter(|| update_diagnostics(&self.ctl.shared, &self.ctl.outer));
        self.ctl.outer.diagnostics()
    }

    /// The primary term this Pod may act under now: it leads a term whose
    /// Lease is fresh by its own monotonic clock. At most one authority in a
    /// cluster returns a term at any moment of true time.
    pub fn primary_term(&self) -> Option<crate::rules::state::Term> {
        self.executor
            .enter(|| self.ctl.shared.primary_term(rt::now()))
    }
}

impl Ctl {
    /// Apply reports and stop requests after a turn. True when finished.
    fn after_turn(&mut self) -> bool {
        let shared = &self.shared;
        let outer = &self.outer;
        let g = outer.report_generation.load(Ordering::SeqCst);
        if g != self.seen_report {
            self.seen_report = g;
            self.report_signal.notify();
        }
        update_diagnostics(shared, outer);
        if outer.stop.load(Ordering::SeqCst) {
            if !shared.stopping.get() {
                shared.stopping.set(true);
                shared.changed.notify();
                self.stop_at = Some(rt::now() + STOP_GRACE);
            }
            // Give seat tasks a bounded chance to release Leases.
            return shared.seat.borrow().is_none()
                && !shared.is_primary.get()
                && shared.releasing.get() == 0
                || self.stop_at.is_some_and(|t| rt::now() >= t);
        }
        false
    }
}

/// Bounded time a stopping authority spends releasing its Leases.
const STOP_GRACE: Duration = Duration::from_secs(5);

impl Outer {
    fn new() -> Arc<Self> {
        Arc::new(Self {
            bootstrap: Mutex::new(None),
            bootstrapped: Condvar::new(),
            report: Mutex::new(None),
            report_generation: AtomicU64::new(0),
            stop: AtomicBool::new(false),
            diagnostics: Mutex::new(Diagnostics::default()),
        })
    }

    fn set_report(
        &self,
        shares: u32,
        rdma_nics: Vec<racer_control_wire::RailMapping>,
    ) -> Result<(), Error> {
        let report = Report {
            shares: NonZeroU32::new(shares).ok_or(Error::InvalidConfiguration)?,
            rdma_nics,
        };
        let encoded = rails::encode_report(&report).map_err(|_| Error::InvalidConfiguration)?;
        *self.report.lock().unwrap_or_else(|e| e.into_inner()) = Some(encoded);
        self.report_generation.fetch_add(1, Ordering::SeqCst);
        Ok(())
    }

    fn diagnostics(&self) -> Diagnostics {
        self.diagnostics
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .clone()
    }
}

fn update_diagnostics(shared: &Shared, outer: &Outer) {
    let role = match (
        shared.seat.borrow().as_ref().map(|s| s.index),
        shared.is_primary.get(),
    ) {
        (Some(i), true) => Role::Primary(i),
        (Some(i), false) => Role::Seat(i),
        _ => Role::Member,
    };
    if let Ok(mut d) = outer.diagnostics.try_lock() {
        *d = Diagnostics {
            role,
            sequence: shared
                .serving
                .borrow()
                .record
                .as_ref()
                .map_or(0, |r| r.sequence),
            ready: shared.ready(),
            tasks: rt::task_count(),
            last_error: shared.last_error.get(),
        };
    }
}

/// Read the cluster ID and this Node's block device pattern once.
async fn bootstrap(shared: Rc<Shared>, outer: Arc<Outer>) {
    let mut backoff = 1;
    loop {
        if shared.stopping.get() {
            return;
        }
        let result = async {
            let secret = shared
                .lease_api
                .get(&paths::state(&shared.config.namespace))
                .await?;
            let uid = str_at(&secret, &["metadata", "uid"]).to_owned();
            if !racer_control_wire::valid_uuid(&uid) {
                return Err(Error::InvalidState);
            }
            let initialized = !str_at(
                &secret,
                &["metadata", "annotations", annotations::INITIALIZED],
            )
            .is_empty();
            if initialized && !seat::state_markers_match(&secret, &uid) {
                return Err(Error::InvalidState);
            }
            // The installation records the racer-state UID it runs on in the
            // DaemonSet and ServiceAccount. A replaced Secret must not become
            // a new cluster identity, even on a fresh Node, so this gate runs
            // before the cluster ID is set, and so before any seat election
            // or state initialization.
            bound_to_installation(&shared, &uid).await?;
            let node = shared
                .lease_api
                .get(&paths::node(&shared.config.node_name))
                .await?;
            let block =
                server::block_devices(&crate::kube::map_at(&node, &["metadata", "annotations"]));
            Ok(Bootstrap {
                cluster: racer_control_wire::ClusterId(uid),
                block_devices: block,
            })
        }
        .await;
        match result {
            Ok(b) => {
                *shared.cluster.borrow_mut() = b.cluster.0.clone();
                *outer.bootstrap.lock().unwrap_or_else(|e| e.into_inner()) = Some(Ok(b));
                outer.bootstrapped.notify_all();
                shared.changed.notify();
                return;
            }
            Err(Error::InvalidState) => {
                *outer.bootstrap.lock().unwrap_or_else(|e| e.into_inner()) =
                    Some(Err(Error::InvalidState));
                outer.bootstrapped.notify_all();
                return;
            }
            Err(_) => {
                rt::sleep(Duration::from_secs(backoff)).await;
                backoff = (backoff * 2).min(30);
            }
        }
    }
}

/// Annotation on the dataplane DaemonSet and ServiceAccount recording the
/// racer-state Secret UID the installation runs on.
pub(crate) const STATE_UID: &str = "unbounded-cloud.io/racer-state-uid";

/// Require the live DaemonSet and ServiceAccount to record `uid`. Missing,
/// terminating, unannotated, or mismatched objects fail closed; transient API
/// errors retry.
async fn bound_to_installation(shared: &Shared, uid: &str) -> Result<(), Error> {
    let ns = &shared.config.namespace;
    for path in [
        paths::daemonset(ns, &shared.config.daemonset),
        paths::service_account(ns, &shared.config.service_account),
    ] {
        let obj = match shared.lease_api.get(&path).await {
            Ok(obj) => obj,
            Err(crate::kube::KubeError::NotFound) => return Err(Error::InvalidState),
            Err(e) => return Err(e.into()),
        };
        if !str_at(&obj, &["metadata", "deletionTimestamp"]).is_empty()
            || str_at(&obj, &["metadata", "annotations", STATE_UID]) != uid
        {
            return Err(Error::InvalidState);
        }
    }
    Ok(())
}

/// Patch this Pod's report annotation whenever a new report arrives.
async fn reporter(shared: Rc<Shared>, outer: Arc<Outer>, signal: Signal) {
    let mut written: Option<String> = None;
    let mut backoff = 1;
    while !shared.stopping.get() {
        let generation = signal.generation();
        let wanted = outer
            .report
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .clone();
        if wanted.is_some() && wanted != written {
            let patch = json!({"metadata": {"annotations": {annotations::REPORT: wanted}}});
            match shared
                .api
                .merge_patch(
                    &paths::pod(&shared.config.namespace, &shared.config.pod_name),
                    &patch,
                )
                .await
            {
                Ok(_) => {
                    written = wanted;
                    backoff = 1;
                }
                Err(_) => {
                    rt::sleep(Duration::from_secs(backoff)).await;
                    backoff = (backoff * 2).min(60);
                    continue;
                }
            }
        }
        let _ = rt::timeout(Duration::from_secs(60), signal.changed(generation)).await;
    }
}

impl Handle {
    /// Wait for the cluster ID and block device pattern. Does not wait for
    /// dataplane enrollment.
    pub fn bootstrap(&self, timeout: Duration) -> Result<Bootstrap, Error> {
        let deadline = Instant::now() + timeout;
        let mut guard = self
            .outer
            .bootstrap
            .lock()
            .unwrap_or_else(|e| e.into_inner());
        loop {
            if let Some(r) = guard.as_ref() {
                return r.clone();
            }
            if self.outer.stop.load(Ordering::SeqCst) {
                return Err(Error::Stopped);
            }
            let now = Instant::now();
            if now >= deadline {
                return Err(Error::Timeout);
            }
            guard = self
                .outer
                .bootstrapped
                .wait_timeout(guard, deadline - now)
                .unwrap_or_else(|e| e.into_inner())
                .0;
        }
    }

    /// Ready seat endpoints, updated from the primary Lease.
    pub fn endpoints(&self) -> DiscoveredEndpoints {
        self.endpoints.clone()
    }

    /// Report shares and RDMA NICs in this Pod's report annotation.
    pub fn report(
        &self,
        shares: u32,
        rdma_nics: Vec<racer_control_wire::RailMapping>,
    ) -> Result<(), Error> {
        self.outer.set_report(shares, rdma_nics)?;
        self.notifier.notify();
        Ok(())
    }

    pub fn diagnostics(&self) -> Diagnostics {
        self.outer.diagnostics()
    }

    /// Stop the thread, releasing held Leases within a bounded time.
    pub fn shutdown(mut self) {
        self.stop();
    }

    fn stop(&mut self) {
        self.outer.stop.store(true, Ordering::SeqCst);
        self.outer.bootstrapped.notify_all();
        self.notifier.notify();
        if let Some(t) = self.thread.take() {
            let deadline = Instant::now() + JOIN_TIMEOUT;
            while !t.is_finished() && Instant::now() < deadline {
                self.notifier.notify();
                std::thread::sleep(Duration::from_millis(20));
            }
            if t.is_finished() {
                let _ = t.join();
            }
        }
    }
}

impl Drop for Handle {
    fn drop(&mut self) {
        self.stop();
    }
}
