//! Runtime orchestration and the public handle.

use crate::{
    Config, DiscoveredEndpoints, Error, annotations,
    authority::rails::{self, Report},
    exec::{self, Executor, Listener, Notifier, Signal},
    kube::{Kube, KubeConfig, paths, str_at},
    limits,
    pki::DynamicClientVerifier,
    seat, server,
    shared::{Serving, Shared},
};
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
    let outer = Arc::new(Outer {
        bootstrap: Mutex::new(None),
        bootstrapped: Condvar::new(),
        report: Mutex::new(None),
        report_generation: AtomicU64::new(0),
        stop: AtomicBool::new(false),
        diagnostics: Mutex::new(Diagnostics::default()),
    });
    let endpoints: DiscoveredEndpoints = Default::default();
    let (tx, rx) = std::sync::mpsc::sync_channel(1);
    let (o, e) = (outer.clone(), endpoints.clone());
    let thread = std::thread::Builder::new()
        .name("racer-control".into())
        .spawn(move || run(config, o, e, tx))
        .map_err(|_| Error::Io)?;
    let notifier = match rx.recv_timeout(START_TIMEOUT) {
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

fn run(
    config: Config,
    outer: Arc<Outer>,
    endpoints: DiscoveredEndpoints,
    ready: std::sync::mpsc::SyncSender<Result<Notifier, Error>>,
) {
    #[allow(clippy::type_complexity)]
    let setup =
        || -> Result<(Executor, Rc<Shared>, Listener, Listener, Option<Listener>), Error> {
            let executor = Executor::new(MAX_TASKS).map_err(|_| Error::Io)?;
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
            getrandom::getrandom(&mut nonce).map_err(|_| Error::Io)?;
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
                verifier: DynamicClientVerifier::new(),
                seat_holders: RefCell::new(Default::default()),
                is_primary: Default::default(),
                primary: RefCell::new(Default::default()),
                stopping: Default::default(),
                releasing: Default::default(),
                last_error: Default::default(),
            });
            Ok((executor, shared, control, webhook, webhook_alternate))
        };
    let (mut executor, shared, control, webhook, webhook_alternate) = match setup() {
        Ok(v) => v,
        Err(e) => {
            let _ = ready.send(Err(e));
            return;
        }
    };
    // Nobody waits any more after a start timeout: exit without serving.
    if ready.send(Ok(executor.notifier())).is_err() || outer.stop.load(Ordering::SeqCst) {
        return;
    }
    let report_signal = Signal::default();
    let primary_changed = Rc::new(RefCell::new((String::new(), None)));
    let started = {
        let shared = shared.clone();
        let outer = outer.clone();
        let report_signal = report_signal.clone();
        move || -> Result<(), Error> {
            exec::spawn(bootstrap(shared.clone(), outer.clone()))?;
            exec::spawn(seat::discovery(
                shared.clone(),
                endpoints.clone(),
                primary_changed.clone(),
            ))?;
            exec::spawn(seat::lottery(shared.clone(), primary_changed.clone()))?;
            exec::spawn(server::Server::new(shared.clone()).run(control))?;
            // One budget across both Pod IP families.
            server::spawn_webhooks(&shared, std::iter::once(webhook).chain(webhook_alternate))?;
            exec::spawn(reporter(shared.clone(), outer.clone(), report_signal))?;
            Ok(())
        }
    };
    let mut started = Some(started);
    let mut seen_report = 0;
    let mut stop_at: Option<Instant> = None;
    executor.run(|| {
        if let Some(start) = started.take()
            && start().is_err()
        {
            return true;
        }
        let g = outer.report_generation.load(Ordering::SeqCst);
        if g != seen_report {
            seen_report = g;
            report_signal.notify();
        }
        update_diagnostics(&shared, &outer);
        if outer.stop.load(Ordering::SeqCst) {
            if !shared.stopping.get() {
                shared.stopping.set(true);
                shared.changed.notify();
                stop_at = Some(Instant::now() + Duration::from_secs(5));
            }
            // Give seat tasks a bounded chance to release Leases.
            return shared.seat.borrow().is_none()
                && !shared.is_primary.get()
                && shared.releasing.get() == 0
                || stop_at.is_some_and(|t| Instant::now() >= t);
        }
        false
    });
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
            tasks: exec::task_count(),
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
                exec::sleep(Duration::from_secs(backoff)).await;
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
                    exec::sleep(Duration::from_secs(backoff)).await;
                    backoff = (backoff * 2).min(60);
                    continue;
                }
            }
        }
        let _ = exec::timeout(Duration::from_secs(60), signal.changed(generation)).await;
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
        let report = Report {
            shares: NonZeroU32::new(shares).ok_or(Error::InvalidConfiguration)?,
            rdma_nics,
        };
        let encoded = rails::encode_report(&report).map_err(|_| Error::InvalidConfiguration)?;
        *self.outer.report.lock().unwrap_or_else(|e| e.into_inner()) = Some(encoded);
        self.outer.report_generation.fetch_add(1, Ordering::SeqCst);
        self.notifier.notify();
        Ok(())
    }

    pub fn diagnostics(&self) -> Diagnostics {
        self.outer
            .diagnostics
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .clone()
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
