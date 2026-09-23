// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Worker-local volume generations over one uniquely owned shard collection.
//!
//! Physical registration is deferred until an authenticated fabric is configured. Only barrier-activated
//! generations negotiate, with one outbound session per selected peer slot and
//! canonical initiator worker shard. HTTP stays usable throughout negotiation.
//! Managers and every live/draining handler are polled even with idle listeners.
//! Negotiation installs transport authentication before exposing Established;
//! manager admission checks generation/policy and never reinstalls the session.
use crate::{
    cache::{Cache, Namespace},
    control::{Decision, Prepared, Updates},
    handlers::{Handler, Peer},
    http::Progress,
    http_server as http, negotiation,
    peer_identity::NodeId,
    rdma,
    socket::Address,
    uring,
};
use std::{
    cell::{Cell, RefCell},
    collections::BTreeMap,
    io,
    net::SocketAddr,
    num::NonZeroU32,
    rc::Rc,
    sync::Arc,
    time::{Duration, Instant},
};

mod generation;
mod listeners;
mod storage;
mod topology;
use generation::Generation;
pub use storage::{StorageCoordinator, StorageHandle, StoragePath, validate_startup_memory};

const NEGOTIATION_TIMEOUT: Duration = Duration::from_secs(5);
const DRAIN_TIMEOUT: Duration = Duration::from_secs(30);
const SHUTDOWN_TIMEOUT: Duration = Duration::from_secs(5);
const MAX_PATHS: usize = 64;
const MAX_DRAINING: usize = 4;
// Node identities, volume/cache generation, shard and exact target are bound
// separately by Context. Geometry/epoch is also bound by negotiation::Context.
const ROUTING: &[u8] = b"racer/runtime/topology/v1";

/// Keep HTTP serving if one registered RNIC fails. The original source remains
/// on its owning reactor and retries quiescence until DMA owners can be released.
pub struct RdmaSource {
    source: rdma::Source,
    failed: bool,
    quiesced: bool,
    retry: Instant,
}
impl RdmaSource {
    pub fn new(source: rdma::Source) -> Self {
        Self {
            source,
            failed: false,
            quiesced: false,
            retry: crate::environment::now(),
        }
    }
    fn failed(&mut self, error: io::Error) {
        if !self.failed {
            eprintln!(
                "RDMA source failed; using HTTP: {error}; recovery=restart: restore fabric/device health then restart process (no live rediscovery)"
            );
        }
        self.failed = true;
    }
}
impl uring::CompletionSource for RdmaSource {
    fn poll(&mut self, ring: &mut uring::Ring, budget: usize) -> io::Result<uring::Work> {
        if !self.failed {
            match self.source.poll(ring, budget) {
                Ok(work) => return Ok(work),
                Err(error) => self.failed(error),
            }
        }
        if !self.quiesced && crate::environment::now() >= self.retry {
            self.quiesced = self.source.shutdown(ring).is_ok();
            // Cleanup is nonblocking. Keep servicing provider event ACKs and
            // completion even after the source has permanently selected HTTP.
            self.retry = crate::environment::now() + Duration::from_millis(10);
        }
        Ok(uring::Work {
            runnable: false,
            deadline: (!self.quiesced).then_some(self.retry),
        })
    }
    fn arm(&mut self, ring: &mut uring::Ring) -> io::Result<()> {
        if !self.failed
            && let Err(error) = self.source.arm(ring)
        {
            self.failed(error);
        }
        Ok(())
    }
    fn shutdown(&mut self, ring: &mut uring::Ring) -> io::Result<()> {
        self.source.shutdown(ring)
    }
}

fn unavailable() -> io::Error {
    io::Error::new(io::ErrorKind::NotConnected, "RDMA path unavailable")
}

struct Retry {
    failures: u32,
    after: Instant,
}
impl Retry {
    fn new(now: Instant) -> Self {
        Self {
            failures: 0,
            after: now,
        }
    }
    fn fail(&mut self, now: Instant) {
        self.after = now + Duration::from_millis(250 * (1u64 << self.failures.min(7)));
        self.failures = self.failures.saturating_add(1);
    }
}
struct Outbound {
    id: String,
    target: String,
    handler: usize,
    client: Option<negotiation::Client>,
    retry: Retry,
}
struct Live {
    connection: Rc<rdma::Connection>,
    context: Rc<negotiation::Context>,
    peer: NodeId,
    handler: usize,
    outbound: Option<usize>,
    confirmation: Option<Instant>,
}
struct Manager {
    context: Rc<negotiation::Context>,
    rails: negotiation::Rails,
    outbound: Vec<Outbound>,
    inbound: BTreeMap<(NodeId, u64), Rc<RefCell<negotiation::Server>>>,
    live: Vec<Live>,
}
impl Manager {
    fn trigger(&mut self, id: &str, target: &str) {
        if self.outbound.iter().any(|p| p.id == id) || self.outbound.len() >= MAX_PATHS {
            return;
        }
        let Some(peer) = self
            .context
            .prepared()
            .eligible_peer_for_volume(self.context.volume_id(), id)
        else {
            return;
        };
        self.outbound.push(Outbound {
            id: peer.id().to_owned(),
            target: target.to_owned(),
            handler: 0,
            client: None,
            retry: Retry::new(crate::environment::now()),
        });
    }
    fn incoming(
        &mut self,
        hint: &negotiation::RequestHint,
    ) -> io::Result<Rc<RefCell<negotiation::Server>>> {
        // Claims choose a canonical shard, never membership, volume or policy.
        if hint.volume != self.context.volume()
            || !self
                .context
                .prepared()
                .rdma_member(self.context.volume_id(), hint.node)
        {
            return Err(unavailable());
        }
        let key = (hint.node, hint.shard);
        let replacement = self
            .live
            .iter()
            .find(|p| {
                p.outbound.is_none() && p.peer == hint.node && p.context.shard() == hint.shard
            })
            .map(|p| p.connection.clone());
        if let Some(server) = self.inbound.get(&key) {
            return Ok(server.clone());
        }
        if hint.is_finish
            || (replacement.is_none()
                && self.inbound.len()
                    + self
                        .live
                        .iter()
                        .filter(|p| {
                            p.outbound.is_none()
                                && !self.inbound.contains_key(&(p.peer, p.context.shard()))
                        })
                        .count()
                    >= MAX_PATHS)
        {
            return Err(unavailable());
        }
        let context = Rc::new(
            negotiation::Context::new(
                self.context.prepared().clone(),
                self.context.volume_id(),
                hint.shard,
                ROUTING,
            )?
            .with_credentials(self.context.credentials())
            .with_authority(self.context.authority()),
        );
        let server = negotiation::Server::new(context, self.rails.clone(), 1, NEGOTIATION_TIMEOUT)?;
        let server = Rc::new(RefCell::new(match replacement {
            Some(old) => server.replacing(old),
            None => server,
        }));
        self.inbound.insert(key, server.clone());
        Ok(server)
    }
    fn pending(&self, id: &http::ConnectionId) -> Option<Rc<RefCell<negotiation::Server>>> {
        self.inbound
            .values()
            .find(|s| s.borrow().has_pending(id))
            .cloned()
    }
    fn admit(
        &mut self,
        established: negotiation::Established,
        outbound: Option<usize>,
        generation: &Generation,
    ) -> io::Result<()> {
        // An old Finish may complete on its original TCP, but cannot install a
        // stale session, even into a newer handler with the same peer identity.
        if generation.expired.get()
            || generation.drain.get().is_some()
            || !Arc::ptr_eq(established.context.prepared(), &generation._config)
        {
            return Err(unavailable());
        }
        let predecessors: Vec<_> = self
            .live
            .iter()
            .filter(|p| {
                p.outbound.is_none() == outbound.is_none()
                    && p.peer == established.peer
                    && p.context.shard() == established.context.shard()
            })
            .collect();
        // One draining predecessor may coexist with its authenticated replacement.
        if self.live.len() >= 4 * MAX_PATHS
            || predecessors.len() >= 2
            || predecessors.iter().any(|p| !p.connection.key_draining())
        {
            return Err(unavailable());
        }
        let connection = Rc::new(established.connection);
        let handler = outbound.map_or(0, |i| self.outbound[i].handler);
        if let Some(i) = outbound {
            self.outbound[i].retry.failures = 0;
            generation.handlers[handler]
                .borrow_mut()
                .set_routed_connection(&self.outbound[i].id, connection.clone());
        } else {
            generation.handlers[handler]
                .borrow_mut()
                .add_shared_connection(connection.clone());
        }
        self.live.push(Live {
            connection,
            context: established.context,
            peer: established.peer,
            handler,
            outbound,
            confirmation: established.confirmation_deadline,
        });
        Ok(())
    }
    fn poll(
        &mut self,
        generation: &Generation,
        ring: &mut uring::Ring,
        budget: usize,
    ) -> uring::Work {
        let now = crate::environment::now();
        let mut work = uring::Work::default();
        let mut i = 0;
        while i < self.live.len() {
            let replaced = self.live[i].connection.is_drained()
                && self.live.iter().enumerate().any(|(other, replacement)| {
                    other != i
                        && replacement.peer == self.live[i].peer
                        && replacement.outbound == self.live[i].outbound
                        && replacement.context.shard() == self.live[i].context.shard()
                        && replacement.connection.is_healthy()
                        && replacement.connection.is_confirmed()
                        && !replacement.connection.key_draining()
                });
            let live = &mut self.live[i];
            if live.connection.is_confirmed() {
                live.confirmation = None;
            }
            if replaced
                || !live.connection.is_healthy()
                || live.confirmation.is_some_and(|d| now >= d)
            {
                let live = self.live.swap_remove(i);
                let _ = live.connection.disconnect();
                generation.handlers[live.handler]
                    .borrow_mut()
                    .remove_connection(&live.connection);
                if let Some(i) = live.outbound.filter(|_| !replaced) {
                    self.outbound[i].retry.fail(now);
                }
            } else {
                work.merge(uring::Work {
                    runnable: false,
                    deadline: live.confirmation,
                });
                i += 1;
            }
        }
        for i in 0..self.outbound.len() {
            if !generation.active.get() {
                self.outbound[i].client = None;
                continue;
            }
            if self
                .live
                .iter()
                .any(|p| p.outbound == Some(i) && !p.connection.key_draining())
            {
                continue;
            }
            let path = &mut self.outbound[i];
            if path.client.is_none() && now >= path.retry.after {
                match negotiation::Client::start(
                    self.context.clone(),
                    self.rails.clone(),
                    &path.id,
                    &path.target,
                    NEGOTIATION_TIMEOUT,
                ) {
                    Ok(client) => path.client = Some(client),
                    Err(_) => path.retry.fail(now),
                }
            }
            if let Some(client) = &mut path.client {
                match client.poll(ring, budget) {
                    Ok(Progress::Pending(w)) => work.merge(w),
                    result => {
                        path.client = None;
                        let admitted = match result {
                            Ok(Progress::Ready(e)) => self.admit(e, Some(i), generation).is_ok(),
                            _ => false,
                        };
                        if !admitted {
                            self.outbound[i].retry.fail(now);
                        }
                        work.runnable = true;
                    }
                }
            }
            if self.outbound[i].client.is_none()
                && !self
                    .live
                    .iter()
                    .any(|p| p.outbound == Some(i) && !p.connection.key_draining())
            {
                work.merge(uring::Work {
                    runnable: false,
                    deadline: Some(self.outbound[i].retry.after),
                });
            }
        }
        let servers: Vec<_> = self.inbound.values().cloned().collect();
        for server in servers {
            let mut server = server.borrow_mut();
            work.merge(server.poll(now));
            while let Some(established) = server.take_completed(now) {
                let _ = self.admit(established, None, generation);
                work.runnable = true;
            }
        }
        self.inbound.retain(|_, s| s.borrow().reserved() != 0);
        // Sources wake on CQ failures; this also bounds locally detected health
        // changes and unconfirmed idle sessions independently of HTTP activity.
        if !self.live.is_empty() {
            work.merge(uring::Work {
                runnable: false,
                deadline: Some(now + Duration::from_millis(100)),
            });
        }
        work
    }
    fn clear(&mut self, handlers: &[Rc<RefCell<Handler>>]) {
        self.outbound.clear();
        for server in self.inbound.values() {
            server.borrow_mut().clear();
        }
        self.inbound.clear();
        for live in self.live.drain(..) {
            let _ = live.connection.disconnect();
            handlers[live.handler]
                .borrow_mut()
                .remove_connection(&live.connection);
        }
    }
}

pub struct VolumeHandler {
    local: bool,
    current: Rc<Generation>,
    draining: Vec<Rc<Generation>>,
}
struct PeerHandler {
    config: Option<Arc<Prepared>>,
    volumes: BTreeMap<String, VolumeHandler>,
}
struct PeerTask(io::Result<(String, Task)>);
impl http::Handler for PeerHandler {
    type Task = PeerTask;
    fn start(&mut self, request: http::Request) -> PeerTask {
        PeerTask((|| {
            let identity = request.peer_identity().ok_or_else(|| {
                io::Error::new(io::ErrorKind::PermissionDenied, "peer TLS identity missing")
            })?;
            let config = self.config.as_ref().ok_or_else(unavailable)?;
            let volume = request
                .headers()
                .get("x-racer-volume")
                .ok_or_else(unavailable)?;
            let volume = std::str::from_utf8(volume)
                .map_err(io::Error::other)?
                .to_owned();
            config.authorize_member(&volume, identity)?;
            let handler = self.volumes.get_mut(&volume).ok_or_else(unavailable)?;
            if !negotiation::is_negotiation(request.headers())
                && matches!(
                    crate::handlers::routing_identity(request.headers()),
                    Ok(None)
                )
            {
                let data = handler.current.handlers[0].clone();
                let task = data.borrow_mut().reject(request, 409);
                return Ok((
                    volume,
                    Task {
                        generation: handler.current.clone(),
                        kind: TaskKind::Data(data, task),
                    },
                ));
            }
            Ok((volume, handler.start(request)))
        })())
    }
    fn poll(
        &mut self,
        task: &mut PeerTask,
        ring: &mut uring::Ring,
        budget: usize,
    ) -> io::Result<Progress<http::Completed>> {
        let (_, task) = task
            .0
            .as_mut()
            .map_err(|e| io::Error::new(e.kind(), e.to_string()))?;
        VolumeHandler {
            local: false,
            current: task.generation.clone(),
            draining: Vec::new(),
        }
        .poll(task, ring, budget)
    }
}
fn hex_identity(bytes: &[u8]) -> String {
    bytes.iter().map(|b| format!("{b:02x}")).collect()
}
pub struct Task {
    generation: Rc<Generation>,
    kind: TaskKind,
}
enum TaskKind {
    Data(Rc<RefCell<Handler>>, crate::handlers::Task),
    Negotiation(Rc<RefCell<negotiation::Server>>, negotiation::Task),
    Failed(Option<io::Error>),
}
impl VolumeHandler {
    fn negotiation(&self, request: http::Request) -> Task {
        let mut generation = self.current.clone();
        let result = (|| {
            let hint = negotiation::request_hint(request.headers())?;
            let mut server = None;
            if hint.is_finish {
                for candidate in std::iter::once(&self.current).chain(&self.draining) {
                    if candidate.expired.get()
                        || candidate
                            .drain
                            .get()
                            .is_some_and(|d| crate::environment::now() >= d)
                    {
                        continue;
                    }
                    if let Some(manager) = &candidate.manager
                        && let Some(pending) = manager.borrow().pending(&request.connection_id())
                    {
                        generation = candidate.clone();
                        server = Some(pending);
                        break;
                    }
                }
            }
            let server = match server {
                Some(server) => server,
                None if !self.current.expired.get() && !hint.is_finish => self
                    .current
                    .manager
                    .as_ref()
                    .ok_or_else(unavailable)?
                    .borrow_mut()
                    .incoming(&hint)?,
                None => return Err(unavailable()),
            };
            let task = server.borrow_mut().start(request)?;
            Ok(TaskKind::Negotiation(server, task))
        })();
        Task {
            generation,
            kind: result.unwrap_or_else(|e| TaskKind::Failed(Some(e))),
        }
    }
    fn poll_background(
        &mut self,
        ring: &mut uring::Ring,
        budget: usize,
    ) -> io::Result<uring::Work> {
        // The local listener owns background polling for generations shared with
        // the dedicated TLS peer dispatcher, including retired listeners.
        let mut work = uring::Work::default();
        work.merge(self.current.poll(ring, budget)?);
        for generation in &self.draining {
            work.merge(generation.poll(ring, budget)?);
        }
        self.draining.retain(|g| !g.expired.get());
        Ok(work)
    }
    fn expire(&mut self) {
        self.current.expire();
        for generation in &self.draining {
            generation.expire();
        }
        self.draining.clear();
    }
}
impl http::Handler for VolumeHandler {
    type Task = Task;
    fn start(&mut self, request: http::Request) -> Task {
        // Ordinary ingress never admits peer protocol without authenticated TLS.
        if request.peer_identity().is_none()
            && (request.headers().get("x-racer-fault").is_some()
                || negotiation::is_negotiation(request.headers()))
        {
            let handler = self.current.handlers[0].clone();
            let task = handler.borrow_mut().reject(request, 403);
            return Task {
                generation: self.current.clone(),
                kind: TaskKind::Data(handler, task),
            };
        }
        if !self.local && negotiation::is_negotiation(request.headers()) {
            return self.negotiation(request);
        }
        let generation = match crate::handlers::routing_identity(request.headers()) {
            // Handler checks the immutable RF06 namespace before rebasing the
            // sender's placement hint onto current local routing.
            Ok(Some(_)) if !self.local => {
                (!self.current.expired.get()).then(|| self.current.clone())
            }
            Ok(None) if self.local && !negotiation::is_negotiation(request.headers()) => {
                self.current.active.get().then(|| self.current.clone())
            }
            _ => None,
        };
        let Some(generation) = generation else {
            let handler = self.current.handlers[0].clone();
            let task = handler.borrow_mut().reject(request, 409);
            return Task {
                generation: self.current.clone(),
                kind: TaskKind::Data(handler, task),
            };
        };

        let handler = generation.handlers[0].clone();

        let task = handler.borrow_mut().start(request);
        Task {
            generation,
            kind: TaskKind::Data(handler, task),
        }
    }
    fn poll(
        &mut self,
        task: &mut Task,
        ring: &mut uring::Ring,
        budget: usize,
    ) -> io::Result<Progress<http::Completed>> {
        if task.generation.expired.get()
            || task
                .generation
                .drain
                .get()
                .is_some_and(|d| crate::environment::now() >= d)
        {
            return Err(unavailable());
        }
        match &mut task.kind {
            TaskKind::Data(handler, task) => handler.borrow_mut().poll(task, ring, budget),
            TaskKind::Negotiation(server, task) => {
                server.borrow_mut().poll_task(task, ring, budget)
            }
            TaskKind::Failed(error) => Err(error.take().unwrap_or_else(unavailable)),
        }
    }
}
impl Volumes {
    /// One immutable process-start catalog. Attempt registration once, only when
    /// useful. SO_REUSEPORT requires the same sparse catalog on every worker.
    pub fn with_rdma_startup(
        mut self,
        policy: rdma::StartupPolicy,
        catalog: Vec<Option<rdma::Rail>>,
    ) -> Self {
        self.rdma_startup = Some((policy, catalog));
        self
    }
    fn provision_rdma(&mut self, config: &Prepared, ring: &uring::Ring) -> io::Result<()> {
        self.provision_rdma_with(config, ring, |rail, config| {
            rdma::Transport::new(ring.pool(), rail, config)
        })
    }
    fn provision_rdma_with(
        &mut self,
        config: &Prepared,
        ring: &uring::Ring,
        mut register: impl FnMut(
            rdma::Rail,
            rdma::Config,
        ) -> io::Result<(rdma::Transport, rdma::Source)>,
    ) -> io::Result<()> {
        if !rdma::StartupPolicy::eligible(
            config.fabric().is_some(),
            self.updates.credentials().is_some() || cfg!(test),
        ) {
            return Ok(());
        }
        let Some((policy, catalog)) = self.rdma_startup.take() else {
            return Ok(());
        };
        if catalog.is_empty() {
            return Ok(());
        }
        let mut transports = Vec::with_capacity(catalog.len());
        for (index, rail) in catalog.into_iter().enumerate() {
            let Some(rail) = rail else {
                transports.push(None);
                continue;
            };
            match register(rail, policy.transport.clone()) {
                Ok((transport, source)) => {
                    transport.set_metrics(ring.metrics().clone())?;
                    transports.push(Some(transport));
                    self.rdma_sources.push((index, RdmaSource::new(source)));
                    eprintln!("worker {}: RDMA rail {index} registered", self.worker);
                }
                Err(error) => {
                    transports.push(None);
                    eprintln!(
                        "worker {}: RDMA rail {index} registration failed; HTTP fallback: {error}; recovery=restart: correct device/resources then restart",
                        self.worker
                    );
                }
            }
        }
        let total = transports.len();
        self.rails = Some(negotiation::Rails::new(transports, total)?);
        Ok(())
    }
}

pub struct Volumes {
    storage: Option<storage::Local>,
    topology_fence: topology::MaintenanceFence,
    receive_authority: Rc<RefCell<Option<Arc<Prepared>>>>,
    peer_server: Option<http::Server<PeerHandler>>,
    credential_revision: u64,
    stopping: bool,
    peer_ip: std::net::IpAddr,
    worker: usize,
    rails: Option<negotiation::Rails>,
    rdma_startup: Option<(rdma::StartupPolicy, Vec<Option<rdma::Rail>>)>,
    rdma_sources: Vec<(usize, RdmaSource)>,
    staged: Option<Staged>,
    // One failed local preparation; successful stages stay owned until decision.
    preparing: Option<(Arc<Prepared>, Retry)>,
    crypto: Arc<crate::crypto::Pool>,
    crypto_sources: Vec<(
        std::rc::Weak<RefCell<crate::crypto::Worker>>,
        crate::crypto::Source,
    )>,
    cache: Rc<RefCell<Cache>>,
    updates: Arc<Updates>,
    revision: u64,
    servers: BTreeMap<Address, http::Server<VolumeHandler>>,
    retired: BTreeMap<Address, (Instant, http::Server<VolumeHandler>)>,
    peer_metrics_deadline: Option<Instant>,
}
struct Staged {
    config: Arc<Prepared>,
    revision: u64,
    generations: BTreeMap<Address, Rc<Generation>>,
    listeners: BTreeMap<Address, http::Listener>,
}
impl Volumes {
    fn install_credentials(&mut self) -> io::Result<()> {
        let Some(provider) = self.updates.credentials() else {
            return Ok(());
        };
        let snapshot = provider.current();
        if self.credential_revision != snapshot.revision {
            let expected = crate::tls::ExpectedPeer::Universe(provider.identity().universe.clone());
            if self.peer_server.is_none() {
                let address = SocketAddr::new(self.peer_ip, 9443);
                let listener = http::Listener::bind(address, NonZeroU32::new(1024).unwrap())?;
                self.peer_server = Some(http::Server::new(
                    listener,
                    PeerHandler {
                        config: None,
                        volumes: BTreeMap::new(),
                    },
                    http::Config::default(),
                ));
            }
            self.peer_server.as_mut().unwrap().install_tls(
                (*snapshot.context).clone(),
                expected,
                snapshot.revision,
                snapshot.expires_unix,
            );

            self.credential_revision = snapshot.revision;
        }
        provider.installed(
            self.worker,
            self.credential_revision,
            crate::http_client::TlsChannel::old_connections(self.credential_revision),
        );
        Ok(())
    }
    pub fn new(
        cache: Cache,
        updates: Arc<Updates>,
        crypto: Arc<crate::crypto::Pool>,
        worker: usize,
    ) -> Self {
        Self {
            storage: None,
            topology_fence: topology::MaintenanceFence::default(),
            receive_authority: Rc::new(RefCell::new(None)),
            peer_server: None,
            credential_revision: 0,
            stopping: false,
            worker,
            peer_ip: std::net::Ipv4Addr::UNSPECIFIED.into(),
            rails: None,
            rdma_startup: None,
            rdma_sources: Vec::new(),
            staged: None,
            preparing: None,
            crypto,
            crypto_sources: Vec::new(),
            cache: Rc::new(RefCell::new(cache)),
            updates,
            revision: 0,
            servers: BTreeMap::new(),
            retired: BTreeMap::new(),
            peer_metrics_deadline: None,
        }
    }
    /// Worker-owned physical catalog, registered before any snapshot is active.
    pub fn with_rdma(mut self, rails: Option<negotiation::Rails>) -> Self {
        self.rails = rails;
        self
    }
    /// Set before polling to the primary Pod IP. Independent of management;
    /// authenticated peer traffic always listens on port 9443.
    pub fn with_peer_ip(mut self, ip: std::net::IpAddr) -> Self {
        self.peer_ip = ip;
        self
    }
    pub fn poll(&mut self, ring: &mut uring::Ring, budget: usize) -> io::Result<uring::Work> {
        self.install_credentials()?;
        let mut work = self.poll_storage(ring)?;
        work.merge(
            self.cache
                .borrow_mut()
                .poll(ring, budget)
                .map_err(io::Error::other)?,
        );
        work.merge(self.poll_topology(ring)?);
        work.merge(self.poll_listeners(ring, budget)?);
        work.merge(self.poll_sources(ring, budget)?);
        work.merge(self.poll_peer_metrics(ring.metrics()));
        Ok(work)
    }
    fn poll_sources(&mut self, ring: &mut uring::Ring, budget: usize) -> io::Result<uring::Work> {
        let mut work = uring::Work::default();
        self.crypto_sources
            .retain(|(worker, _)| worker.strong_count() != 0);
        for (_, source) in &mut self.crypto_sources {
            work.merge(uring::CompletionSource::poll(source, ring, budget)?);
        }
        for (index, source) in &mut self.rdma_sources {
            let failed = source.failed;
            work.merge(uring::CompletionSource::poll(source, ring, budget)?);
            uring::CompletionSource::arm(source, ring)?;
            // Close the CQ notification race before the application's reactor sleeps.
            work.merge(uring::CompletionSource::poll(source, ring, budget)?);
            if !failed && source.failed {
                eprintln!(
                    "worker {}: RDMA rail {index} stopped; HTTP fallback; restart required",
                    self.worker
                );
            }
        }
        Ok(work)
    }
    fn poll_peer_metrics(&mut self, metrics: &crate::metrics::Local) -> uring::Work {
        let now = crate::environment::now();
        if self.peer_metrics_deadline.is_none_or(|at| now >= at) {
            let mut peers = Vec::new();
            for server in self.servers.values() {
                let generation = &server.handler().current;
                if !generation.active.get() || generation.expired.get() {
                    continue;
                }
                for handler in &generation.handlers {
                    handler
                        .borrow()
                        .peer_metrics(&generation.volume, &mut peers);
                }
            }
            metrics.publish_peers(peers);
            self.peer_metrics_deadline = Some(now + crate::metrics::INTERVAL);
        }
        uring::Work {
            runnable: false,
            deadline: self.peer_metrics_deadline,
        }
    }
    pub fn shutdown(&mut self, ring: &mut uring::Ring) -> io::Result<()> {
        self.shutdown_until(ring, crate::environment::now() + SHUTDOWN_TIMEOUT)
    }
    fn shutdown_until(&mut self, ring: &mut uring::Ring, deadline: Instant) -> io::Result<()> {
        if let Some(storage) = &self.storage {
            storage.stop();
        }
        if let Some(mut peer) = self.peer_server.take() {
            peer.shutdown(ring)?;
        }
        self.stopping = true;
        self.staged = None;
        self.preparing = None;
        for server in self
            .servers
            .values_mut()
            .chain(self.retired.values_mut().map(|(_, s)| s))
        {
            server.handler_mut().expire();
            server.shutdown(ring)?;
        }
        self.servers.clear();
        self.retired.clear();
        ring.metrics().publish_peers(Vec::new());
        self.crypto_sources.clear();
        let mut cache_done = false;
        let mut error = None;
        loop {
            let mut work = uring::Work {
                runnable: ring.progress()?,
                deadline: Some(deadline),
            };
            // Cache completion must not wait behind a pending or failed provider.
            // Keep the ring open and service event ACKs on this same owner until
            // destruction actually completes; EAGAIN is normal helper progress.
            if !cache_done {
                match self.cache.borrow_mut().poll_shutdown(ring) {
                    Ok((done, progress)) => {
                        cache_done = done;
                        work.merge(progress);
                    }
                    Err(e) => {
                        error.get_or_insert_with(|| io::Error::other(e));
                        cache_done = true;
                    }
                }
            }
            let now = crate::environment::now();
            for (_, source) in &mut self.rdma_sources {
                if source.quiesced {
                    continue;
                }
                if now >= source.retry {
                    match uring::CompletionSource::shutdown(source, ring) {
                        Ok(()) => source.quiesced = true,
                        Err(e) => {
                            let delay = if e.kind() == io::ErrorKind::WouldBlock {
                                10
                            } else {
                                100
                            };
                            source.retry = now + Duration::from_millis(delay);
                        }
                    }
                }
                if !source.quiesced {
                    work.merge(uring::Work {
                        runnable: false,
                        deadline: Some(source.retry),
                    });
                }
            }
            if cache_done && self.rdma_sources.iter().all(|(_, s)| s.quiesced) {
                self.rdma_sources.clear();
                return error.map_or(Ok(()), Err);
            }
            if crate::environment::now() >= deadline {
                // Retain every unquiesced transport and DMA lease. The driver's
                // ring teardown and conservative transport Drop remain safe.
                return Err(error.unwrap_or_else(|| {
                    io::Error::new(io::ErrorKind::TimedOut, "volume shutdown did not quiesce")
                }));
            }
            if !work.runnable {
                ring.wait(work.deadline)?;
            }
        }
    }
    pub fn begin_drain(&mut self) {
        if let Some(storage) = &self.storage {
            storage.stop();
        }
        if let Some(peer) = &mut self.peer_server {
            peer.begin_drain();
        }
        self.stopping = true;
        self.staged = None;
        self.preparing = None;
        for server in self
            .servers
            .values_mut()
            .chain(self.retired.values_mut().map(|(_, s)| s))
        {
            server.begin_drain();
            let handler = server.handler_mut();
            for generation in std::iter::once(&handler.current).chain(&handler.draining) {
                generation.active.set(false);
                for handler in &generation.handlers {
                    handler.borrow_mut().begin_drain();
                }
            }
        }
    }
    pub fn drained(&self) -> bool {
        self.peer_server
            .as_ref()
            .is_none_or(|s| s.connections() == 0)
            && self
                .servers
                .values()
                .chain(self.retired.values().map(|(_, s)| s))
                .all(|s| s.connections() == 0)
    }
}

#[cfg(test)]
#[path = "../tests/runtime/listeners.rs"]
mod listener_tests;
#[cfg(test)]
#[path = "../tests/runtime/activation.rs"]
pub(crate) mod staging_tests;
#[cfg(test)]
#[path = "../tests/runtime/scenarios.rs"]
pub(crate) mod tests;

pub(crate) mod environment {
    //! Operating system sources of time and entropy.
    pub(crate) fn now() -> std::time::Instant {
        std::time::Instant::now()
    }
    pub(crate) fn wall() -> std::time::SystemTime {
        std::time::SystemTime::now()
    }
    pub(crate) fn random(bytes: &mut [u8]) -> Result<(), getrandom::Error> {
        getrandom::getrandom(bytes)
    }
}
