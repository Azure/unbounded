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
    control::{Prepared, Updates},
    handlers::{Handler, Peer},
    http::Progress,
    http_server as http, negotiation,
    peer_identity::NodeId,
    rdma, uring,
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

mod storage;
pub use storage::{StorageCoordinator, StorageHandle, StoragePath};

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
            || self
                .context
                .prepared()
                .eligible_node_for_volume(self.context.volume_id(), hint.node)
                .is_none()
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
        let context = Rc::new(negotiation::Context::new(
            self.context.prepared().clone(),
            self.context.volume_id(),
            hint.shard,
            ROUTING,
        )?);
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
        if !generation.active.get()
            || !Arc::ptr_eq(established.context.prepared(), &generation._config)
        {
            return Err(unavailable());
        }
        if self.live.len() >= 2 * MAX_PATHS
            || self.live.iter().any(|p| {
                p.outbound.is_none() == outbound.is_none()
                    && p.peer == established.peer
                    && p.context.shard() == established.context.shard()
            })
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
            let live = &mut self.live[i];
            if live.connection.is_confirmed() {
                live.confirmation = None;
            }
            if !live.connection.is_healthy() || live.confirmation.is_some_and(|d| now >= d) {
                let live = self.live.swap_remove(i);
                let _ = live.connection.disconnect();
                generation.handlers[live.handler]
                    .borrow_mut()
                    .remove_connection(&live.connection);
                if let Some(i) = live.outbound {
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
            if self.live.iter().any(|p| p.outbound == Some(i)) {
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
            if self.outbound[i].client.is_none() && !self.live.iter().any(|p| p.outbound == Some(i))
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

struct Generation {
    handlers: Vec<Rc<RefCell<Handler>>>,
    _config: Arc<Prepared>,
    manager: Option<RefCell<Manager>>,
    active: Cell<bool>,
    drain: Cell<Option<Instant>>,
    expired: Cell<bool>,
    identity: [u8; 32],
}
impl Generation {
    fn retire(&self, now: Instant) {
        self.active.set(false);
        // Reusing a removed listener must not renew its old generations' leases.
        if self.drain.get().is_none() {
            self.drain.set(Some(now + DRAIN_TIMEOUT));
        }
        if let Some(manager) = &self.manager {
            for path in &mut manager.borrow_mut().outbound {
                path.client = None;
            }
        }
    }
    fn expire(&self) {
        self.active.set(false);
        self.expired.set(true);
        if let Some(manager) = &self.manager {
            manager.borrow_mut().clear(&self.handlers);
        }
    }
    fn poll(&self, ring: &mut uring::Ring, budget: usize) -> io::Result<uring::Work> {
        if self
            .drain
            .get()
            .is_some_and(|d| crate::environment::now() >= d)
        {
            self.expire();
        }
        if self.expired.get() {
            return Ok(uring::Work::default());
        }
        let mut work = uring::Work {
            runnable: false,
            deadline: self.drain.get(),
        };
        if let Some(manager) = &self.manager {
            work.merge(manager.borrow_mut().poll(self, ring, budget));
        }
        for handler in &self.handlers {
            let mut handler = handler.borrow_mut();
            work.merge(handler.poll_background(ring, budget)?);
            let negotiations = handler.take_negotiations();
            if self.active.get()
                && let Some(manager) = &self.manager
            {
                for (id, target) in negotiations {
                    manager.borrow_mut().trigger(&id, &target);
                    work.runnable = true;
                }
            }
        }
        Ok(work)
    }
}
pub struct VolumeHandler {
    current: Rc<Generation>,
    draining: Vec<Rc<Generation>>,
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
                None if self.current.active.get() && !hint.is_finish => self
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
        let mut work = self.current.poll(ring, budget)?;
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
        if negotiation::is_negotiation(request.headers()) {
            return self.negotiation(request);
        }
        let generation = match crate::handlers::routing_identity(request.headers()) {
            Ok(Some(identity)) => std::iter::once(&self.current)
                .chain(&self.draining)
                .find(|g| !g.expired.get() && g.identity == identity)
                .cloned(),
            Ok(None) => self.current.active.get().then(|| self.current.clone()),
            Err(_) => None,
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
// Worker-local socket reconciliation, independent of generation authority.

impl Volumes {
    // Validate the entire candidate before any bind/crypto attachment. A listener
    // already accepts kernel traffic while merely staged, so rollback after a
    // conflicting bind would be too late. Only exact active/retired keys permit
    // socket reuse; aliases of those keys are not reusable socket identities.
    fn validate_listeners(&self, config: &Prepared) -> io::Result<()> {
        for (index, volume) in config.volumes.iter().enumerate() {
            let address = volume.address;
            let conflict = config.volumes[..index]
                .iter()
                .map(|v| (v.address, "candidate"))
                .chain(
                    self.servers
                        .keys()
                        .filter(|&&a| a != address)
                        .map(|&a| (a, "active")),
                )
                .chain(
                    self.retired
                        .keys()
                        .filter(|&&a| a != address)
                        .map(|&a| (a, "retired")),
                )
                // An unarmed stage owns its sockets until abort/supersession.
                // It cannot be overwritten by another preparation, even at an
                // exact key: prepare only reuses active/retired servers.
                .chain(
                    self.staged
                        .iter()
                        .flat_map(|s| s.listeners.keys().map(|&a| (a, "staged"))),
                )
                .find(|&(other, _)| crate::listener_policy::overlaps(address, other));
            if let Some((other, state)) = conflict {
                return Err(io::Error::new(
                    io::ErrorKind::AddrInUse,
                    format!(
                        "volume {} listener {address} overlaps {state} listener {other}",
                        volume.config.id
                    ),
                ));
            }
        }
        Ok(())
    }

    // Move the whole server: outstanding accept, accepted sockets and tasks all
    // keep their original ring and pinned generations. Never bind a competitor.
    fn reclaim_listener(&mut self, address: SocketAddr) {
        if let Some((_, server)) = self.retired.remove(&address) {
            assert!(!self.servers.contains_key(&address));
            self.servers.insert(address, server);
        }
    }

    fn commit(&mut self, staged: Staged) {
        let Staged {
            generations,
            mut listeners,
            ..
        } = staged;
        let now = crate::environment::now();
        // Everything that can fail has completed. No requests are polled between
        // these mutations. Socket ownership is reconciled by address, not volume ID.
        let removed: Vec<_> = self
            .servers
            .keys()
            .filter(|a| !generations.contains_key(a))
            .copied()
            .collect();
        for address in removed {
            let mut server = self.servers.remove(&address).unwrap();
            server.handler_mut().current.retire(now);
            // Old peers remain addressable; ordinary ingress is disabled.
            assert!(
                self.retired
                    .insert(address, (now + DRAIN_TIMEOUT, server))
                    .is_none()
            );
        }
        for (address, current) in generations {
            self.reclaim_listener(address);
            current.active.set(true);
            if let Some(server) = self.servers.get_mut(&address) {
                let handler = server.handler_mut();
                handler
                    .draining
                    .retain(|g| !Rc::ptr_eq(g, &current) && !g.expired.get());
                if Rc::ptr_eq(&handler.current, &current) {
                    continue;
                }
                handler.current.retire(now);
                let old = std::mem::replace(&mut handler.current, current);
                if !old.expired.get() {
                    handler.draining.push(old);
                }
                if handler.draining.len() > MAX_DRAINING {
                    handler.draining.remove(0).expire();
                }
            } else {
                self.servers.insert(
                    address,
                    http::Server::new(
                        listeners.remove(&address).unwrap(),
                        VolumeHandler {
                            current,
                            draining: Vec::new(),
                        },
                        http::Config::default(),
                    ),
                );
            }
        }
    }

    fn arm(&mut self, staged: &mut Staged) {
        if staged.armed {
            return;
        }
        for (address, candidate) in &staged.generations {
            self.reclaim_listener(*address);
            if let Some(server) = self.servers.get_mut(address) {
                server.handler_mut().draining.push(candidate.clone());
            } else {
                self.servers.insert(
                    *address,
                    http::Server::new(
                        staged.listeners.remove(address).unwrap(),
                        VolumeHandler {
                            current: candidate.clone(),
                            draining: Vec::new(),
                        },
                        http::Config::default(),
                    ),
                );
            }
        }
        staged.armed = true;
        self.updates.received(staged.revision, self.worker);
    }

    fn poll_retired(&mut self, ring: &mut uring::Ring, budget: usize) -> io::Result<uring::Work> {
        let now = crate::environment::now();
        let mut work = uring::Work::default();
        let mut closed = Vec::new();
        for (address, (deadline, server)) in &mut self.retired {
            let reserved = self
                .staged
                .as_ref()
                .is_some_and(|stage| stage.generations.contains_key(address));
            if now >= *deadline && !reserved {
                server.handler_mut().expire();
                server.shutdown(ring)?;
                closed.push(*address);
            } else {
                // A ready, unarmed stage reserves the socket, not old authority.
                // Expire generations at their original deadlines even while the
                // stage waits. Abort/supersession releases the reservation above.
                work.merge(server.handler_mut().poll_background(ring, budget)?);
                work.merge(server.poll(ring, budget)?);
                if now < *deadline {
                    work.merge(uring::Work {
                        runnable: false,
                        deadline: Some(*deadline),
                    });
                }
            }
        }
        for address in closed {
            self.retired.remove(&address);
        }
        Ok(work)
    }
}
// Process-local listener reservation, checked at worker staging so rejected
// desired listeners remain visible to aggregate readiness.

fn validate_management(config: &Prepared, management: SocketAddr) -> io::Result<()> {
    // Deliberately conservative across interfaces and address families. Do not
    // depend on wildcard/dual-stack bind behavior or SO_REUSEPORT selection.
    for volume in &config.volumes {
        if volume.address.port() == management.port() {
            return Err(io::Error::new(
                io::ErrorKind::AddrInUse,
                format!(
                    "volume {} listener {} conflicts with management {management} (reserved port)",
                    volume.config.id, volume.address
                ),
            ));
        }
    }
    Ok(())
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
            config.crypto_snapshot().signatures().can_authenticate(),
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
    maintenance: bool,
    stopping: bool,
    management: SocketAddr,
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
    servers: BTreeMap<SocketAddr, http::Server<VolumeHandler>>,
    retired: BTreeMap<SocketAddr, (Instant, http::Server<VolumeHandler>)>,
    peer_metrics_deadline: Option<Instant>,
}
struct Staged {
    revision: u64,
    generations: BTreeMap<SocketAddr, Rc<Generation>>,
    listeners: BTreeMap<SocketAddr, http::Listener>,
    armed: bool,
}
impl Volumes {
    pub fn new(
        cache: Cache,
        updates: Arc<Updates>,
        crypto: Arc<crate::crypto::Pool>,
        worker: usize,
    ) -> Self {
        Self {
            storage: None,
            maintenance: false,
            stopping: false,
            worker,
            management: SocketAddr::from(([0, 0, 0, 0], 9090)),
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
    /// Set before polling, using the process Exporter's actual bound address.
    pub fn with_management(mut self, address: SocketAddr) -> Self {
        self.management = address;
        self
    }
    fn prepare(&mut self, config: Arc<Prepared>, ring: &mut uring::Ring) -> io::Result<()> {
        validate_management(&config, self.management)?;
        self.validate_listeners(&config)?;
        self.provision_rdma(&config, ring)?;
        let crypto = {
            let (worker, source) = self.crypto.attach_local(ring.pool(), ring.wake_handle())?;
            let worker = Rc::new(RefCell::new(worker));
            self.crypto_sources.push((Rc::downgrade(&worker), source));
            Some(worker)
        };
        let mut generations = BTreeMap::new();
        let mut listeners = BTreeMap::new();
        for volume in &config.volumes {
            if !self.servers.contains_key(&volume.address)
                && !self.retired.contains_key(&volume.address)
            {
                listeners.insert(
                    volume.address,
                    http::Listener::bind(volume.address, NonZeroU32::new(1024).unwrap())?,
                );
            }
            let namespace = Namespace::volume(
                &config.config.universe,
                &volume.config.id,
                volume.config.cache_generation,
                volume.backend.namespace(),
            );
            let mut handler =
                Handler::shared(self.cache.clone(), volume.backend.clone(), namespace);
            handler.set_crypto(crypto.clone());
            handler.set_authentication(crate::http_auth::Policy {
                keys: config.crypto.signatures().clone(),
                universe: config.crypto.universe().bytes(),
                node: config.local_node().bytes(),
                peers: volume
                    .peers
                    .keys()
                    .filter_map(|p| {
                        p.parse::<crate::peer_identity::NodeId>()
                            .ok()
                            .map(|n| n.bytes())
                    })
                    .collect(),
            });
            handler.set_attempt_policy(volume.config.max_candidate_attempts.unwrap_or(3))?;
            handler.set_routing(
                volume.routing.clone(),
                volume
                    .config
                    .peers
                    .iter()
                    .map(|id| (id.clone(), Peer::from_endpoint(volume.peers[id].clone())))
                    .collect(),
            );
            let handlers = vec![Rc::new(RefCell::new(handler))];
            generations.insert(
                volume.address,
                Rc::new(Generation {
                    identity: volume.routing.identity,
                    handlers,
                    _config: config.clone(),
                    manager: if let Some(rails) = &self.rails
                        && config.fabric().is_some()
                        && config.crypto_snapshot().signatures().can_authenticate()
                    {
                        Some(RefCell::new(Manager {
                            context: Rc::new(negotiation::Context::new(
                                config.clone(),
                                &volume.config.id,
                                self.worker as u64,
                                ROUTING,
                            )?),
                            rails: rails.clone(),
                            outbound: Vec::new(),
                            inbound: BTreeMap::new(),
                            live: Vec::new(),
                        }))
                    } else {
                        None
                    },
                    active: Cell::new(false),
                    drain: Cell::new(None),
                    expired: Cell::new(false),
                }),
            );
        }
        self.staged = Some(Staged {
            revision: config.config.revision,
            generations,
            listeners,
            armed: false,
        });
        Ok(())
    }
    pub fn poll(&mut self, ring: &mut uring::Ring, budget: usize) -> io::Result<uring::Work> {
        let mut work = self.poll_storage(ring)?;
        work.merge(
            self.cache
                .borrow_mut()
                .poll(ring, budget)
                .map_err(io::Error::other)?,
        );
        if !self.maintenance {
            if !self.stopping
                && let Some(config) = self.updates.latest(self.revision)
            {
                self.staged = None;
                self.revision = config.config.revision;
                self.preparing = Some((config, Retry::new(crate::environment::now())));
            }
            if let Some((config, mut retry)) = self.preparing.take()
                && self.updates.decision(self.revision) != Some(false)
            {
                // Subscription progress (including 304) is independent of this timer.
                // Preparation is synchronous: retained ready stages never re-ack an
                // older attempt, and Updates fences superseded/aborted revisions.
                let now = crate::environment::now();
                if now >= retry.after {
                    match self.prepare(config.clone(), ring) {
                        Ok(()) => self.updates.staged(self.revision, self.worker, true),
                        Err(error) => {
                            eprintln!("volume activation failed: {error}");
                            self.updates.staged(self.revision, self.worker, false);
                            retry.fail(crate::environment::now());
                        }
                    }
                }
                if self.staged.is_none() {
                    work.merge(uring::Work {
                        runnable: false,
                        deadline: Some(retry.after),
                    });
                    self.preparing = Some((config, retry));
                }
            }
            if let Some(mut staged) = self.staged.take() {
                if self.updates.receive_decision(staged.revision) {
                    self.arm(&mut staged);
                }
                match self.updates.decision(staged.revision) {
                    Some(true) => {
                        self.commit(staged);
                        self.updates.activated(self.revision, self.worker);
                    }
                    Some(false) => {}
                    None => self.staged = Some(staged),
                }
            }
        }
        for server in self.servers.values_mut() {
            work.merge(server.poll(ring, budget)?);
            work.merge(server.handler_mut().poll_background(ring, budget)?);
        }
        work.merge(self.poll_retired(ring, budget)?);
        self.cache.borrow_mut().set_crypto(None);
        if self.retired.is_empty()
            && self
                .servers
                .values_mut()
                .all(|s| s.handler_mut().draining.is_empty())
        {
            self.updates.retired(self.revision, self.worker);
        }
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
        work.merge(self.poll_peer_metrics(ring.metrics()));
        Ok(work)
    }
    fn poll_peer_metrics(&mut self, metrics: &crate::metrics::Local) -> uring::Work {
        let now = crate::environment::now();
        if self.peer_metrics_deadline.is_none_or(|at| now >= at) {
            let mut peers = Vec::new();
            for (address, server) in &self.servers {
                let generation = &server.handler().current;
                if !generation.active.get() || generation.expired.get() {
                    continue;
                }
                let volume = generation
                    ._config
                    .volumes
                    .iter()
                    .find(|volume| volume.address == *address)
                    .unwrap();
                for handler in &generation.handlers {
                    handler.borrow().peer_metrics(&volume.config.id, &mut peers);
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
        self.cache.borrow_mut().set_crypto(None);
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
        self.servers
            .values()
            .chain(self.retired.values().map(|(_, s)| s))
            .all(|s| s.connections() == 0)
    }
}

#[cfg(test)]
#[path = "../tests/runtime/cluster.rs"]
mod dst;
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
    //! Runtime sources of time and entropy. Production always uses the OS; the
    //! test-only simulator installs a scoped, thread-local deterministic world.
    pub(crate) fn now() -> std::time::Instant {
        #[cfg(test)]
        if let Some(world) = crate::simulation::current() {
            return world.now();
        }
        std::time::Instant::now()
    }
    pub(crate) fn wall() -> std::time::SystemTime {
        #[cfg(test)]
        if let Some(world) = crate::simulation::current() {
            return world.wall();
        }
        std::time::SystemTime::now()
    }
    pub(crate) fn random(bytes: &mut [u8]) -> Result<(), getrandom::Error> {
        #[cfg(test)]
        if let Some(world) = crate::simulation::current() {
            world.random(bytes);
            return Ok(());
        }
        getrandom::getrandom(bytes)
    }
}
