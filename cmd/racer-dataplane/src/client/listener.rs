//! Owned per-cache Unix sockets with permissions, bounded connections, and draining.
//! Bind /run/racer/<cache name>/client/socket; mount its client directory separately
//! from the origin directory so pods receive only their authorized endpoint.

use super::RequestParser;
use super::Responses;
use super::handle_read_result;
use crate::admission::AdmissionPolicy;
use crate::error::Error;
use crate::error::Operation;
use crate::error::Result;
use crate::http::ConnectionLease;
use crate::http::HttpIo;
use crate::model::RequestId;
use crate::read::Coordinator;
use crate::runtime::Cancellation;
use crate::runtime::RequestScope;
use racer_control_wire::CacheDefinition;
use racer_control_wire::CacheId;
use std::cell::Cell;
use std::cell::RefCell;
use std::collections::BTreeMap;
use std::collections::BTreeSet;
use std::collections::VecDeque;
use std::fs::File;
use std::os::fd::AsRawFd;
use std::path::Path;
use std::path::PathBuf;
use std::rc::Rc;
use std::sync::Arc;
use std::task::Context;
use std::task::Poll;
use std::time::Duration;
#[cfg(test)]
pub(super) use uds_endpoint::file_path;
use uds_endpoint::publication::{Endpoint, Publication, Rename, Replacement};
#[cfg(test)]
pub(super) use uds_endpoint::same_inode;
use uring_runtime::reactor::ready_set::ReadySet;

enum Listener {
    Real(uds_endpoint::BoundSocket),
    #[cfg(test)]
    Sim(uds_endpoint::simulation::BoundSocket),
}
impl Listener {
    fn accept(&self) -> std::io::Result<(uring_runtime::reactor::descriptor::Descriptor, ())> {
        #[cfg(test)]
        if let Some(errno) = FAIL_ACCEPT.with(|fail| fail.take()) {
            return Err(std::io::Error::from_raw_os_error(errno));
        }
        match self {
            Self::Real(listener) => listener.accept().map(|(socket, _)| (socket.into(), ())),
            #[cfg(test)]
            Self::Sim(socket) => socket.accept().map(|fd| (fd, ())),
        }
    }
}
enum Directory {
    Real(Rc<File>),
    #[cfg(test)]
    Sim,
}

pub(super) struct BoundListener {
    definition: CacheDefinition,
    listener: Listener,
    directory: Directory,
    retired: Arc<std::sync::atomic::AtomicBool>,
    backlog_allowed: Arc<std::sync::atomic::AtomicBool>,
    pub(super) owner: Option<Rc<EndpointOwner>>,
}

// UnixListener's backlog is bounded by the kernel. Also cap total attempts so a
// privileged peer using an old witness cannot keep a retired listener alive.
const RETIRE_ACCEPT_LIMIT: usize = 256;
pub(super) const MAX_RETIRING_LISTENERS: usize = 64;

/// Only the retirement accept path can mint this single-request allowance. It
/// preserves the old UID, never bypasses Coordinator availability, and remains
/// revocable while queued in a destination handoff.
pub(crate) struct RetirementAuthorization {
    cache: CacheId,
    retired: Arc<std::sync::atomic::AtomicBool>,
    allowed: Arc<std::sync::atomic::AtomicBool>,
    deadline: std::time::Instant,
    source_stopped: Arc<std::sync::atomic::AtomicBool>,
}

enum RetirementReservation {
    Local(crate::admission::ConnectionReservation),
    Remote(crate::admission::Offer),
}

// Weak records retain revocation reachability, not socket FDs or queued tokens.
// A record counts toward the aggregate generation limit while either its
// listener survives or an unexpired handoff still holds the permission flag.
struct RetirementRecord {
    cache: CacheId,
    name: String,
    listener: std::rc::Weak<BoundListener>,
    allowed: std::sync::Weak<std::sync::atomic::AtomicBool>,
    deadline: std::time::Instant,
}

impl RetirementRecord {
    fn live(&self, now: std::time::Instant) -> bool {
        self.listener.strong_count() != 0
            || (now < self.deadline && self.allowed.strong_count() != 0)
    }

    fn revoke(&self) {
        if let Some(allowed) = self.allowed.upgrade() {
            allowed.store(false, std::sync::atomic::Ordering::Release);
        }
    }
}
struct Retiring {
    listener: Rc<BoundListener>,
    remaining: usize,
    deadline: std::time::Instant,
    restricted: bool,
}

impl Retiring {
    fn new(listener: Rc<BoundListener>, deadline: std::time::Instant) -> Self {
        Self {
            listener,
            remaining: RETIRE_ACCEPT_LIMIT,
            deadline,
            restricted: false,
        }
    }
}

impl Drop for BoundListener {
    fn drop(&mut self) {
        self.retired
            .store(true, std::sync::atomic::Ordering::Release);
        // Both BoundSocket backends clean up before the endpoint owner is released.
    }
}

impl BoundListener {
    fn set_mode(&self, mode: u32) -> std::io::Result<()> {
        #[cfg(test)]
        if FAIL_CHMOD.with(|fail| fail.replace(false)) {
            return Err(std::io::Error::other("injected chmod failure"));
        }
        match &self.listener {
            Listener::Real(socket) => socket.set_mode(mode),
            #[cfg(test)]
            Listener::Sim(socket) => socket.set_mode(mode),
        }
    }
}

pub(super) struct Active {
    pub(super) runnable: Arc<uring_runtime::drivers::Runnable>,
    pub(super) deadline: Rc<Cell<std::time::Instant>>,
    pub(super) expired: Option<std::time::Instant>,
    pub(super) cache: CacheId,
    pub(super) retired: Arc<std::sync::atomic::AtomicBool>,
    pub(super) idle: Rc<Cell<bool>>,
    pub(super) cancellation: Cancellation,
    pub(super) operation: Operation<'static, ()>,
}

pub struct ClientListeners {
    generation: Rc<Cell<u64>>,
    readiness: RefCell<ReadyListeners>,
    sim_cursor: RefCell<Option<CacheId>>,
    ingress: Option<Arc<crate::admission::Ingress>>,
    pub(super) metrics: crate::telemetry::Metrics,
    pub(super) reads: Rc<Coordinator>,
    pub(super) parser: RequestParser,
    pub(super) responses: Rc<Responses>,
    pub(super) io: Rc<HttpIo>,
    pub(super) admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
    pub(super) listeners: Rc<RefCell<BTreeMap<CacheId, Rc<BoundListener>>>>,
    pub(super) preparing: Rc<Cell<bool>>,
    cleanup: Rc<RefCell<VecDeque<Rc<BoundListener>>>>,
    retiring: Rc<RefCell<VecDeque<Retiring>>>,
    retirement_records: Rc<RefCell<Vec<RetirementRecord>>>,
    retire_turn: Cell<bool>,
    source_stopped: Arc<std::sync::atomic::AtomicBool>,
    pub(super) active: RefCell<VecDeque<Active>>,
    pub(super) accepting: Rc<Cell<bool>>,
    accept_turn: Cell<bool>,
    pub(super) root: PathBuf,
    request_timeout: Duration,
    // Observe the exact dispatched scopes without replacing acquisition or polling.
    #[cfg(test)]
    pub(super) read_scopes: Rc<RefCell<Vec<RequestScope>>>,
}
impl ClientListeners {
    pub fn new(
        reads: Rc<Coordinator>,
        parser: RequestParser,
        responses: Rc<Responses>,
        io: Rc<HttpIo>,
        admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
    ) -> Self {
        Self {
            generation: Rc::new(Cell::new(0)),
            readiness: RefCell::new(ReadyListeners::default()),
            sim_cursor: RefCell::new(None),
            ingress: None,
            metrics: crate::telemetry::Metrics::default(),
            reads,
            parser,
            responses,
            io,
            admission,
            listeners: Rc::new(RefCell::new(BTreeMap::new())),
            preparing: Rc::new(Cell::new(false)),
            cleanup: Rc::new(RefCell::new(VecDeque::new())),
            retiring: Rc::new(RefCell::new(VecDeque::new())),
            retirement_records: Rc::new(RefCell::new(Vec::new())),
            retire_turn: Cell::new(true),
            source_stopped: Arc::new(std::sync::atomic::AtomicBool::default()),
            active: RefCell::new(VecDeque::new()),
            accepting: Rc::new(Cell::new(true)),
            accept_turn: Cell::new(true),
            root: PathBuf::from("/run/racer"),
            request_timeout: Duration::from_secs(30),
            #[cfg(test)]
            read_scopes: Rc::new(RefCell::new(Vec::new())),
        }
    }
    /// Bound header/idle admission and initial metadata/first-page work. After
    /// headers, distinct pages receive this acquisition budget independently;
    /// socket writes use Delivery's progress-based stall timeout.
    pub fn with_request_timeout(mut self, timeout: Duration) -> Self {
        self.request_timeout = timeout;
        self
    }
    pub fn with_metrics(mut self, metrics: crate::telemetry::Metrics) -> Self {
        self.metrics = metrics;
        self
    }
    pub(crate) fn with_ingress(mut self, ingress: Arc<crate::admission::Ingress>) -> Self {
        self.ingress = Some(ingress);
        self
    }
    /// Install either a locally accepted socket or a distributed ingress handoff.
    /// A queued handoff may arrive after its listener generation was retired.
    pub(crate) fn install_connection(
        &self,
        connection: ConnectionLease,
        cache: CacheId,
        retired: Arc<std::sync::atomic::AtomicBool>,
    ) -> Result<()> {
        if retired.load(std::sync::atomic::Ordering::Acquire) || !self.accepting.get() {
            return Ok(());
        }
        self.install(connection, cache, retired, false)
    }

    fn install(
        &self,
        connection: ConnectionLease,
        cache: CacheId,
        retired: Arc<std::sync::atomic::AtomicBool>,
        retirement_request: bool,
    ) -> Result<()> {
        let cancellation = Cancellation::new()?;
        let task_cancellation = cancellation.clone();
        let task_retired = retired.clone();
        let idle = Rc::new(Cell::new(!retirement_request));
        let task_idle = idle.clone();
        let timeout = self.request_timeout;
        let deadline = Rc::new(Cell::new(uring_runtime::environment::now() + timeout));
        let task_deadline = deadline.clone();
        let parser = self.parser.clone();
        let reads = self.reads.clone();
        let responses = self.responses.clone();
        let io = self.io.clone();
        let admission = self.admission.clone();
        let task_cache = cache.clone();
        let metrics = self.metrics.clone();
        #[cfg(test)]
        let read_scopes = self.read_scopes.clone();
        let operation = Box::pin(async move {
            serve_connection(
                connection,
                &task_cache,
                &parser,
                &reads,
                &responses,
                &io,
                &admission,
                task_cancellation,
                task_retired,
                task_idle,
                timeout,
                &metrics,
                task_deadline,
                retirement_request,
                #[cfg(test)]
                &read_scopes,
            )
            .await
        });
        self.active.borrow_mut().push_back(Active {
            runnable: uring_runtime::drivers::Runnable::new(),
            deadline,
            expired: None,
            cache,
            retired,
            idle,
            cancellation,
            operation,
        });
        Ok(())
    }

    pub(crate) fn install_retirement(
        &self,
        connection: ConnectionLease,
        authorization: RetirementAuthorization,
    ) -> Result<()> {
        if !self.accepting.get()
            || authorization
                .source_stopped
                .load(std::sync::atomic::Ordering::Acquire)
            || !authorization
                .allowed
                .load(std::sync::atomic::Ordering::Acquire)
            || uring_runtime::environment::now() >= authorization.deadline
        {
            return Ok(());
        }
        let cache = authorization.cache.clone();
        if let Err(error) =
            self.install(connection, authorization.cache, authorization.retired, true)
        {
            eprintln!(
                "racer-dataplane: stage=client-retirement operation=install cache={} error={error}",
                cache.0
            );
        }
        Ok(())
    }

    #[cfg(any(test, feature = "subscription-interop"))]
    pub(crate) fn set_root(&mut self, root: PathBuf) {
        self.root = root;
    }
    #[cfg(test)]
    pub(crate) fn request_timeout(&self) -> Duration {
        self.request_timeout
    }

    pub fn reconcile<'a>(
        &'a self,
        caches: &'a [CacheDefinition],
        scope: &'a RequestScope,
    ) -> Operation<'a, ()> {
        Box::pin(async move {
            self.prepare(caches, scope).await?.commit();
            self.cleanup.borrow_mut().clear();
            Ok(())
        })
    }

    /// Perform all fallible filesystem work before synchronous publication.
    /// Dropping the returned transition restores the last-good owned paths.
    pub fn prepare<'a>(
        &'a self,
        definitions: &'a [CacheDefinition],
        scope: &'a RequestScope,
    ) -> Operation<'a, PreparedListeners> {
        Box::pin(async move {
            scope.check()?;
            racer_control_wire::validate_definitions(definitions)?;
            if !self.accepting.get() {
                return Err(Error::Unavailable);
            }
            if self.preparing.replace(true) {
                return Err(Error::Overloaded);
            }
            let mut prepared = PreparedListeners {
                generation: self.generation.clone(),
                definitions: definitions.to_vec(),
                target: self.listeners.clone(),
                next: BTreeMap::new(),
                publication: Publication::default(),
                preparing: self.preparing.clone(),
                accepting: self.accepting.clone(),
                cleanup: self.cleanup.clone(),
                retiring: self.retiring.clone(),
                retirement_records: self.retirement_records.clone(),
                new_records: Vec::new(),
                retirement_timeout: self.request_timeout,
                exchanged: BTreeSet::new(),
                retained_names: definitions.iter().map(|d| d.name.clone()).collect(),
            };
            // Finish older deferred unlinks before reusing a removed cache name.
            self.cleanup.borrow_mut().clear();
            self.retirement_records
                .borrow_mut()
                .retain(|record| record.live(uring_runtime::environment::now()));
            let old = self.listeners.borrow().clone();
            let old_by_name: BTreeMap<_, _> = old
                .values()
                .map(|listener| (listener.definition.name.as_str(), listener.clone()))
                .collect();
            prepared.exchanged = definitions
                .iter()
                .filter_map(|definition| {
                    old_by_name
                        .get(definition.name.as_str())
                        .filter(|old| old.definition != *definition)
                        .map(|old| old.definition.id.clone())
                })
                .collect();
            let retained = self
                .retirement_records
                .borrow()
                .iter()
                .filter(|entry| prepared.retained_names.contains(&entry.name))
                .count();
            if retained.saturating_add(prepared.exchanged.len()) > MAX_RETIRING_LISTENERS {
                return Err(Error::Overloaded);
            }
            self.retirement_records
                .borrow_mut()
                .try_reserve(prepared.exchanged.len())
                .map_err(|_| Error::Overloaded)?;
            prepared
                .new_records
                .try_reserve(prepared.exchanged.len())
                .map_err(|_| Error::Overloaded)?;
            for id in &prepared.exchanged {
                let listener = &old[id];
                prepared.new_records.push(RetirementRecord {
                    cache: id.clone(),
                    name: listener.definition.name.clone(),
                    listener: Rc::downgrade(listener),
                    allowed: Arc::downgrade(&listener.backlog_allowed),
                    deadline: uring_runtime::environment::now(),
                });
            }
            // Commit must not allocate while adding deferred owners to the queue.
            self.cleanup
                .borrow_mut()
                .try_reserve(
                    old.len()
                        .saturating_mul(2)
                        .saturating_add(definitions.len())
                        .saturating_add(self.retiring.borrow().len()),
                )
                .map_err(|_| Error::Overloaded)?;
            self.retiring
                .borrow_mut()
                .try_reserve(old.len())
                .map_err(|_| Error::Overloaded)?;
            let mut pending = Vec::new();
            // Bind and chmod every changed socket before touching any active pathname.
            for definition in definitions {
                scope.check()?;
                if let Some(current) = old
                    .get(&definition.id)
                    .filter(|current| current.definition == *definition)
                {
                    if !current.owns_checked(endpoint_layout()?.canonical())? {
                        return Err(BoundListener::ownership_error());
                    }
                    if let (Some(owner), Directory::Real(directory)) =
                        (&current.owner, &current.directory)
                    {
                        owner.validate(directory)?;
                    }
                    prepared.next.insert(definition.id.clone(), current.clone());
                    continue;
                }
                let mut random = [0; 16];
                uring_runtime::environment::fill_random(&mut random)
                    .map_err(|_| Error::Unavailable)?;
                let temporary = format!(".racer-{:032x}", u128::from_ne_bytes(random));
                let previous = old_by_name.get(definition.name.as_str()).cloned();
                let next = Rc::new(bind(
                    &self.root,
                    definition.clone(),
                    &temporary,
                    previous.as_deref(),
                ).inspect_err(|error| {
                    eprintln!("racer-dataplane: stage=client-endpoint operation=bind cache={} path={} error={error}",
                        definition.id.0, self.root.join(&definition.name).join("client").display());
                })?);
                let replacement = Replacement::prepare(
                    next.clone(),
                    previous,
                    temporary,
                    endpoint_layout()?.canonical().into(),
                )?;
                prepared.next.insert(definition.id.clone(), next.clone());
                pending.push(replacement);
                let mut yielded = false;
                std::future::poll_fn(|cx| {
                    if yielded {
                        Poll::Ready(())
                    } else {
                        yielded = true;
                        cx.waker().wake_by_ref();
                        Poll::Pending
                    }
                })
                .await;
            }
            for replacement in pending {
                scope.check()?;
                prepared.publication.publish(replacement)?;
            }
            scope.check()?;
            Ok(prepared)
        })
    }
    /// Called by the owning I/O worker alongside reactor completion polling.
    /// Work is bounded across accepts and futures; no background executor exists.
    /// Forward the driver's real context. The bounded worker tick covers deadlines,
    /// nonblocking accepts, and round-robin entries beyond this pass's budget.
    pub fn poll_budgeted(&self, cx: &mut Context<'_>, budget: usize) -> Result<usize> {
        let mut cleaned = 0;
        // Commit only retires owners; pathname cleanup runs on worker polling.
        if budget != 0
            && let Some(listener) = self.cleanup.borrow_mut().pop_front()
        {
            drop(listener);
            cleaned = 1;
        }
        // Alternate at budget one so retirement cannot starve completion owners.
        // One retirement step performs at most one nonblocking accept.
        if budget > cleaned
            && !self.retiring.borrow().is_empty()
            && (budget - cleaned > 1 || self.retire_turn.get())
        {
            self.poll_retiring(cx);
            cleaned += 1;
            self.retire_turn.set(false);
        } else if budget > cleaned {
            self.retire_turn.set(true);
        }
        let budget = budget.saturating_sub(cleaned);
        let mut worked = 0;
        let reserve_accept = usize::from(
            self.accepting.get()
                && !self.listeners.borrow().is_empty()
                && (budget > 1 || self.accept_turn.get()),
        );
        if budget == 1 {
            self.accept_turn.set(!self.accept_turn.get());
        }
        let count = self
            .active
            .borrow()
            .len()
            .min(budget.saturating_sub(reserve_accept));
        for _ in 0..count {
            let Some(mut active) = self.active.borrow_mut().pop_front() else {
                break;
            };
            if active.retired.load(std::sync::atomic::Ordering::Acquire) && active.idle.get() {
                let _ = active.cancellation.cancel();
            }
            let deadline = active.deadline.get();
            let force = active.expired != Some(deadline)
                && (uring_runtime::environment::now() >= deadline
                    || active.cancellation.is_cancelled());
            if force {
                active.expired = Some(deadline);
            }
            match active
                .runnable
                .poll(std::pin::Pin::new(&mut active.operation), cx, force)
            {
                Poll::Pending => self.active.borrow_mut().push_back(active),
                Poll::Ready(_) => {}
            }
            worked += 1;
        }
        if !self.accepting.get() {
            return Ok(worked + cleaned);
        }
        let count = self.listeners.borrow().len();
        if count == 0 {
            return Ok(worked + cleaned);
        }
        let maximum = self
            .admission
            .limit(crate::admission::ResourceClass::IngressConnection);
        // Reserve some acceptance opportunity even when all active readers wait.
        let attempts = budget.saturating_sub(worked).min(count);
        for _ in 0..attempts {
            if self
                .admission
                .used(crate::admission::ResourceClass::IngressConnection)
                >= maximum
                && self.ingress.is_none()
            {
                break;
            }
            #[cfg(not(test))]
            let simulated = false;
            #[cfg(test)]
            let simulated = uring_runtime::reactor::simulation::Simulation::current().is_some();
            let listener = if simulated {
                let listeners = self.listeners.borrow();
                let key = self.sim_cursor.borrow().clone();
                let selected = key
                    .as_ref()
                    .and_then(|key| {
                        listeners
                            .range((std::ops::Bound::Excluded(key), std::ops::Bound::Unbounded))
                            .next()
                    })
                    .or_else(|| listeners.first_key_value());
                let Some((key, listener)) = selected else {
                    break;
                };
                *self.sim_cursor.borrow_mut() = Some(key.clone());
                listener.clone()
            } else {
                let Some(listener) =
                    self.readiness
                        .borrow_mut()
                        .next(self, cx, budget.saturating_sub(worked))?
                else {
                    break;
                };
                listener
            };
            let offer = if let Some(ingress) = &self.ingress {
                match ingress.reserve(cx.waker()) {
                    Ok(offer) => Some(offer),
                    Err(Error::Overloaded) => break,
                    Err(error) => return Err(error),
                }
            } else {
                None
            };
            match listener.listener.accept() {
                Ok((socket, _)) => {
                    if let Some(offer) = offer {
                        let socket = socket
                            .into_host()
                            .map_err(|_| Error::InvalidConfiguration)?;
                        offer.deliver(
                            socket,
                            crate::admission::Kind::Client(
                                listener.definition.id.clone(),
                                listener.retired.clone(),
                            ),
                        )?;
                        worked += 1;
                        continue;
                    }
                    let connection = match crate::http::from_accepted(socket, &self.admission) {
                        Ok(connection) => connection,
                        Err(Error::Overloaded) => break,
                        Err(error) => return Err(error),
                    };
                    self.install_connection(
                        connection,
                        listener.definition.id.clone(),
                        listener.retired.clone(),
                    )?;
                    // This operation has not been polled and cannot have registered
                    // an I/O wake yet. Continue promptly without exceeding the budget.
                    cx.waker().wake_by_ref();
                }
                Err(error) if error.kind() == std::io::ErrorKind::WouldBlock => {}
                Err(error) if error.kind() == std::io::ErrorKind::Interrupted => {}
                Err(error) => return Err(directory_error(error)),
            }
            worked += 1;
        }
        Ok(worked + cleaned)
    }

    fn poll_retiring(&self, cx: &mut Context<'_>) {
        let Some(mut entry) = self.retiring.borrow_mut().pop_front() else {
            return;
        };
        if !self.accepting.get()
            || entry.remaining == 0
            || uring_runtime::environment::now() >= entry.deadline
        {
            return;
        }
        if !entry.restricted {
            // The witness is another hard link to this same pinned inode. Restrict
            // both names before draining; root/same-UID peers remain outside the
            // endpoint security boundary, so the attempt/deadline bounds still apply.
            if let Err(error) = entry.listener.set_mode(0) {
                self.retirement_error(&entry, "restrict", error);
                return;
            }
            entry.restricted = true;
        }
        // Reserve atomically before accept, including the destination's total and
        // ingress quotas. A usage precheck races shared ingress reservations.
        let reservation = if let Some(ingress) = &self.ingress {
            ingress
                .reserve(cx.waker())
                .map(RetirementReservation::Remote)
        } else {
            crate::admission::reserve_connection(
                &self.admission,
                crate::admission::ResourceClass::IngressConnection,
            )
            .map(RetirementReservation::Local)
        };
        let reservation = match reservation {
            Ok(reservation) => reservation,
            Err(Error::Overloaded) => {
                self.retiring.borrow_mut().push_back(entry);
                return;
            }
            Err(error) => {
                self.retirement_error(&entry, "reserve", error);
                return;
            }
        };
        entry.remaining -= 1;
        match entry.listener.listener.accept() {
            Ok((socket, _)) => {
                let authorization = RetirementAuthorization {
                    cache: entry.listener.definition.id.clone(),
                    retired: entry.listener.retired.clone(),
                    allowed: entry.listener.backlog_allowed.clone(),
                    deadline: entry.deadline,
                    source_stopped: self.source_stopped.clone(),
                };
                let result = match reservation {
                    RetirementReservation::Remote(offer) => socket
                        .into_host()
                        .map_err(|_| Error::InvalidConfiguration)
                        .and_then(|fd| {
                            offer.deliver(fd, crate::admission::Kind::Retirement(authorization))
                        }),
                    RetirementReservation::Local(reservation) => {
                        crate::http::from_reserved(socket, reservation).and_then(|connection| {
                            self.install_retirement(connection, authorization)
                        })
                    }
                };
                if let Err(error) = result {
                    self.retirement_error(&entry, "install", error);
                }
                if entry.remaining != 0 {
                    self.retiring.borrow_mut().push_back(entry);
                }
                cx.waker().wake_by_ref();
            }
            Err(error) if error.kind() == std::io::ErrorKind::WouldBlock => {}
            Err(error) if error.kind() == std::io::ErrorKind::Interrupted => {
                if entry.remaining != 0 {
                    self.retiring.borrow_mut().push_back(entry);
                    cx.waker().wake_by_ref();
                }
            }
            Err(error) => {
                self.retirement_error(&entry, "accept", &error);
                if matches!(
                    error.raw_os_error(),
                    Some(
                        libc::ECONNABORTED
                            | libc::EMFILE
                            | libc::ENFILE
                            | libc::ENOBUFS
                            | libc::ENOMEM
                    )
                ) && entry.remaining != 0
                {
                    self.retiring.borrow_mut().push_back(entry);
                }
            }
        }
    }

    fn retirement_error(&self, entry: &Retiring, operation: &str, error: impl std::fmt::Display) {
        eprintln!(
            "racer-dataplane: stage=client-retirement operation={operation} cache={} path={} error={error}",
            entry.listener.definition.id.0,
            self.root
                .join(&entry.listener.definition.name)
                .join("client")
                .display()
        );
    }

    pub fn active_connections(&self) -> usize {
        self.active.borrow().len()
    }

    #[cfg(test)]
    pub fn active_connections_for(&self, cache: &CacheId) -> usize {
        self.active
            .borrow()
            .iter()
            .filter(|active| &active.cache == cache)
            .count()
    }

    /// Stop this cache's admission without filesystem I/O. Outstanding responses
    /// finish normally unless cancel_cache is also called.
    pub fn stop_cache(&self, cache: &CacheId) {
        for record in self
            .retirement_records
            .borrow()
            .iter()
            .filter(|record| &record.cache == cache)
        {
            record.revoke();
        }
        // Explicit stop/removal does not authorize new requests from its backlog.
        for entry in self.retiring.borrow_mut().iter_mut() {
            if &entry.listener.definition.id == cache {
                entry.remaining = 0;
                entry
                    .listener
                    .backlog_allowed
                    .store(false, std::sync::atomic::Ordering::Release);
            }
        }
        if let Some(listener) = self.listeners.borrow_mut().remove(cache) {
            self.generation.set(self.generation.get().wrapping_add(1));
            listener
                .retired
                .store(true, std::sync::atomic::Ordering::Release);
            self.cleanup.borrow_mut().push_back(listener);
        }
        for active in self
            .active
            .borrow()
            .iter()
            .filter(|active| &active.cache == cache)
        {
            active
                .retired
                .store(true, std::sync::atomic::Ordering::Release);
        }
    }

    /// Signal every accepted generation for this UID and retain its completion
    /// owners until the worker polls them out.
    pub fn cancel_cache(&self, cache: &CacheId) -> Result<()> {
        self.stop_cache(cache);
        for active in self
            .active
            .borrow()
            .iter()
            .filter(|active| &active.cache == cache)
        {
            active.cancellation.cancel()?;
        }
        Ok(())
    }

    pub fn stop_admission(&self) {
        self.accepting.set(false);
        self.source_stopped
            .store(true, std::sync::atomic::Ordering::Release);
        *self.readiness.borrow_mut() = ReadyListeners::default();
        for (_, listener) in std::mem::take(&mut *self.listeners.borrow_mut()) {
            listener
                .retired
                .store(true, std::sync::atomic::Ordering::Release);
            self.cleanup.borrow_mut().push_back(listener);
        }
    }

    pub fn drain<'a>(&'a self, scope: &'a RequestScope) -> Operation<'a, ()> {
        Box::pin(async move {
            self.stop_admission();
            std::future::poll_fn(|cx| {
                if let Err(error) = scope.check() {
                    for active in self.active.borrow().iter() {
                        let _ = active.cancellation.cancel();
                    }
                    // Keep completion owners until the worker polls them out.
                    return Poll::Ready(Err(error));
                }
                if let Err(error) = self.poll_budgeted(cx, 64) {
                    return Poll::Ready(Err(error));
                }
                if self.active.borrow().is_empty()
                    && self.retiring.borrow().is_empty()
                    && self.cleanup.borrow().is_empty()
                {
                    Poll::Ready(Ok(()))
                } else {
                    cx.waker().wake_by_ref();
                    Poll::Pending
                }
            })
            .await
        })
    }
}

#[allow(clippy::too_many_arguments)]
async fn serve_connection(
    mut connection: ConnectionLease,
    cache: &CacheId,
    parser: &RequestParser,
    reads: &Coordinator,
    responses: &Responses,
    io: &HttpIo,
    admission: &flow_control::Quotas<AdmissionPolicy>,
    cancellation: Cancellation,
    retired: Arc<std::sync::atomic::AtomicBool>,
    idle: Rc<Cell<bool>>,
    timeout: Duration,
    metrics: &crate::telemetry::Metrics,
    deadline: Rc<Cell<std::time::Instant>>,
    retirement_request: bool,
    #[cfg(test)] read_scopes: &RefCell<Vec<RequestScope>>,
) -> Result<()> {
    loop {
        if !retirement_request && retired.load(std::sync::atomic::Ordering::Acquire) {
            return Ok(());
        }
        idle.set(!retirement_request);
        // Parsed headers and opaque context outlive HTTP staging. Charge their
        // bounded representation until the read and every stream slice complete.
        let _context = admission.reserve(
            Some(cache),
            crate::admission::ResourceClass::RequestContext,
            super::MAX_HEAD_BYTES,
        )?;
        let idle_scope = new_scope(timeout, cancellation.clone())?;
        deadline.set(idle_scope.deadline.0);
        let received = io
            .receive_request_head_limited(connection, &idle_scope, parser.header_limit())
            .await?;
        connection = received.connection;
        if !retirement_request && retired.load(std::sync::atomic::Ordering::Acquire) {
            return Ok(());
        }
        idle.set(false);
        let mut observation = metrics.request()?;
        // Idle/header admission has its own bound. A healthy pooled connection
        // gets a fresh operation budget only after each complete request head.
        let scope = new_scope(timeout, cancellation.clone())?;
        deadline.set(scope.deadline.0);
        let scope = &scope;
        let request = match received.value.and_then(|head| parser.parse(cache, head)) {
            Ok(request) => request,
            Err(error) => {
                observation.fail(error);
                responses.send_error(connection, error, scope).await?;
                return Ok(());
            }
        };
        let kind = request.kind.clone();
        let object = request.origin.object.clone();
        let socket = connection.socket();
        let watch_scope = RequestScope {
            body_deadlines: None,
            request: scope.request,
            deadline: scope.deadline,
            cancellation: Cancellation::new()?,
        };
        let mut disconnected = Some(io.disconnected(&connection, &watch_scope));
        let mut disconnect_observed = false;
        #[cfg(test)]
        read_scopes.borrow_mut().push(scope.clone());
        let mut read = reads.read(request, scope);
        let read_result = std::future::poll_fn(|cx| {
            let hangup = match disconnected.as_mut().map(|f| f.as_mut().poll(cx)) {
                Some(Poll::Ready(result)) => {
                    disconnected = None;
                    result.is_ok_and(|events| events & libc::POLLHUP as u32 != 0)
                }
                _ => false,
            };
            if !disconnect_observed && (socket.peer_disconnected() || hangup) {
                disconnect_observed = true;
                let _ = scope.cancel();
            }
            read.as_mut().poll(cx)
        })
        .await;
        let _ = watch_scope.cancel();
        if let Some(watch) = disconnected.take() {
            let _ = watch.await;
        }
        drop(disconnected);
        drop(read);
        connection = match handle_read_result(
            connection,
            &kind,
            &object,
            read_result,
            responses,
            admission,
            scope,
            &mut observation,
            timeout,
        )
        .await?
        {
            Some(connection) => connection,
            None => return Ok(()),
        };
        drop(socket);
        if retirement_request || !connection.is_reusable() {
            return Ok(());
        }
        drop(observation);
        let mut yielded = false;
        std::future::poll_fn(|cx| {
            if yielded {
                Poll::Ready(())
            } else {
                yielded = true;
                cx.waker().wake_by_ref();
                Poll::Pending
            }
        })
        .await;
    }
}

pub(super) fn new_scope(timeout: Duration, cancellation: Cancellation) -> Result<RequestScope> {
    let mut id = [0; 16];
    uring_runtime::environment::fill_random(&mut id).map_err(|_| Error::Unavailable)?;
    Ok(RequestScope {
        body_deadlines: None,
        request: RequestId(id),
        deadline: uring_runtime::environment::Deadline(uring_runtime::environment::now() + timeout),
        cancellation,
    })
}

// Level-triggered listener index. Idle turns wait on one completion-owned poll.
#[derive(Default)]
struct ReadyListeners {
    generation: u64,
    index: Option<ReadySet<Error>>,
    listeners: Vec<std::rc::Weak<BoundListener>>,
    scope: Option<RequestScope>,
}
impl Drop for ReadyListeners {
    fn drop(&mut self) {
        if let Some(scope) = &self.scope {
            let _ = scope.cancel();
        }
    }
}
impl ReadyListeners {
    fn next(
        &mut self,
        owner: &ClientListeners,
        cx: &mut Context<'_>,
        budget: usize,
    ) -> Result<Option<Rc<BoundListener>>> {
        if self.generation != owner.generation.get() {
            if let Some(scope) = self.scope.take() {
                scope.cancel()?;
            }
            if let Some(index) = &mut self.index {
                index.clear();
            }
            self.listeners = owner
                .listeners
                .borrow()
                .values()
                .map(Rc::downgrade)
                .collect();
            let mut ready = ReadySet::new()?;
            for (index, listener) in self.listeners.iter().enumerate() {
                let listener = listener.upgrade().ok_or(Error::Unavailable)?;
                // Test builds also have simulated listeners that must be rejected.
                #[allow(clippy::infallible_destructuring_match)]
                let socket = match &listener.listener {
                    Listener::Real(socket) => socket,
                    #[cfg(test)]
                    _ => return Err(Error::InvalidConfiguration),
                };
                ready.insert(socket.as_raw_fd(), index)?;
            }
            self.index = Some(ready);
            self.scope = Some(new_scope(
                Duration::from_secs(365 * 24 * 3600),
                Cancellation::new()?,
            )?);
            self.generation = owner.generation.get();
        }
        let Some(index) = &mut self.index else {
            return Ok(None);
        };
        let ready = index.next(cx, budget, |fd| {
            let reactor = owner.io.reactor().clone();
            let scope = self.scope.clone();
            Box::pin(async move {
                let scope = scope.ok_or(Error::Internal)?;
                uring_runtime::drivers::retry_listener(&scope, || {
                    reactor.readiness_with_lease(
                        fd.clone(),
                        libc::POLLIN as u32,
                        fd.clone(),
                        &scope,
                    )
                })
                .await
            })
        })?;
        Ok(ready.and_then(|index| self.listeners[index].upgrade()))
    }
}

/// Owns prepared paths and sockets. Keep this on the listener's worker. Drop
/// rolls back; commit performs no filesystem or network operations.
pub struct PreparedListeners {
    generation: Rc<Cell<u64>>,
    definitions: Vec<CacheDefinition>,
    target: Rc<RefCell<BTreeMap<CacheId, Rc<BoundListener>>>>,
    next: BTreeMap<CacheId, Rc<BoundListener>>,
    publication: Publication<BoundListener>,
    preparing: Rc<Cell<bool>>,
    accepting: Rc<Cell<bool>>,
    cleanup: Rc<RefCell<VecDeque<Rc<BoundListener>>>>,
    retiring: Rc<RefCell<VecDeque<Retiring>>>,
    retirement_records: Rc<RefCell<Vec<RetirementRecord>>>,
    new_records: Vec<RetirementRecord>,
    retirement_timeout: Duration,
    exchanged: BTreeSet<CacheId>,
    retained_names: BTreeSet<String>,
}

impl PreparedListeners {
    pub fn definitions(&self) -> &[CacheDefinition] {
        &self.definitions
    }

    /// Infallible activation for the prepared cache publication handoff.
    /// The application serializes prepare/commit with cache stop/drain operations.
    /// Worker polling performs deferred pathname cleanup after this returns.
    pub fn commit(mut self) {
        self.generation.set(self.generation.get().wrapping_add(1));
        let mut target = self.target.borrow_mut();
        let mut cleanup = self.cleanup.borrow_mut();
        self.retirement_records.borrow_mut().retain(|record| {
            let retain = self.accepting.get() && self.retained_names.contains(&record.name);
            if !retain {
                record.revoke();
            }
            retain
        });
        // Removal revokes queued allowances and defers obsolete owners to cleanup.
        // The next prepare clears cleanup before trying to reacquire the lock.
        self.retiring.borrow_mut().retain(|entry| {
            let retain = self.accepting.get()
                && self
                    .retained_names
                    .contains(&entry.listener.definition.name);
            if !retain {
                entry
                    .listener
                    .backlog_allowed
                    .store(false, std::sync::atomic::Ordering::Release);
                cleanup.push_back(entry.listener.clone());
            }
            retain
        });
        if self.accepting.get() {
            let retirement_deadline = uring_runtime::environment::now() + self.retirement_timeout;
            for mut record in self.new_records.drain(..) {
                record.deadline = retirement_deadline;
                self.retirement_records.borrow_mut().push(record);
            }
            let previous = std::mem::replace(&mut *target, std::mem::take(&mut self.next));
            for (id, listener) in previous {
                if !target
                    .get(&id)
                    .is_some_and(|next| Rc::ptr_eq(next, &listener))
                {
                    listener
                        .retired
                        .store(true, std::sync::atomic::Ordering::Release);
                    // Only exchanged endpoints have a backlog to preserve across
                    // publication. Removed caches keep the explicit-stop behavior.
                    if self.exchanged.contains(&id) {
                        self.retiring
                            .borrow_mut()
                            .push_back(Retiring::new(listener, retirement_deadline));
                    } else {
                        cleanup.push_back(listener);
                    }
                }
            }
        } else {
            // Shutdown may overtake preparation, but must never revive admission.
            // stop_admission's cleanup may already have been polled. The journal
            // can be the last owner of an exchanged old socket; defer it too.
            for previous in self.publication.previous() {
                cleanup.push_back(previous.clone());
            }
            for (_, listener) in std::mem::take(&mut self.next) {
                listener
                    .retired
                    .store(true, std::sync::atomic::Ordering::Release);
                cleanup.push_back(listener);
            }
        }
        self.publication.commit();
    }
}

impl crate::control::CacheTransition for PreparedListeners {
    fn commit(self: Box<Self>) {
        (*self).commit();
    }
}

impl Drop for PreparedListeners {
    fn drop(&mut self) {
        self.publication.rollback();
        self.preparing.set(false);
    }
}

pub(super) fn open_directory(path: &Path) -> Result<File> {
    uds_endpoint::open_directory(path, 0o755).map_err(directory_error)
}

pub(super) fn child_directory(parent: &File, name: &[u8]) -> Result<File> {
    uds_endpoint::child_directory(parent, name, 0o755).map_err(directory_error)
}

fn directory_error(error: std::io::Error) -> Error {
    // Racer's Copy boundary error cannot retain an io::Error payload. Log before
    // mapping so semantic ownership failures and syscall errno remain actionable.
    eprintln!(
        "racer-dataplane: stage=client-endpoint error={error} errno={:?}",
        error.raw_os_error()
    );
    if error.kind() == std::io::ErrorKind::InvalidInput && error.raw_os_error().is_none() {
        Error::InvalidConfiguration
    } else {
        Error::Io
    }
}

pub(super) fn prepare_client_directory(directory: &File) -> Result<()> {
    uds_endpoint::restrict_directory(directory, 0o022).map_err(directory_error)
}

fn bind(
    root: &Path,
    definition: CacheDefinition,
    basename: &str,
    previous: Option<&BoundListener>,
) -> Result<BoundListener> {
    #[cfg(test)]
    if let Some(sim) = uring_runtime::reactor::simulation::Simulation::current() {
        let directory = root.join(&definition.name).join("client");
        let listener =
            uds_endpoint::simulation::BoundSocket::bind(sim, directory.clone(), basename)
                .map_err(|_| Error::Io)?;
        let bound = BoundListener {
            definition,
            listener: Listener::Sim(listener),
            directory: Directory::Sim,
            retired: Arc::new(std::sync::atomic::AtomicBool::default()),
            backlog_allowed: Arc::new(std::sync::atomic::AtomicBool::new(true)),
            owner: None,
        };
        if FAIL_CHMOD.with(|fail| fail.replace(false)) {
            return Err(Error::Io);
        }
        bound.set_mode(0o666).map_err(directory_error)?;
        return Ok(bound);
    }
    let root = open_directory(root)?;
    let cache = child_directory(&root, definition.name.as_bytes())?;
    let directory = child_directory(&cache, b"client")?;
    prepare_client_directory(&directory)?;
    let owner = match previous.and_then(|listener| listener.owner.as_ref()) {
        Some(owner) => {
            owner.validate(&directory)?;
            owner.clone()
        }
        None => Rc::new(EndpointOwner::acquire(&directory)?),
    };
    let directory = Rc::new(directory);
    let listener =
        uds_endpoint::BoundSocket::bind(owner.0.clone(), basename).map_err(directory_error)?;
    #[cfg(test)]
    if FAIL_CHMOD.with(|fail| fail.replace(false)) {
        return Err(Error::Io);
    }
    // Pod mounts control access, not process umask or the client's UID/GID.
    listener.set_mode(0o666).map_err(directory_error)?;
    let bound = BoundListener {
        definition,
        listener: Listener::Real(listener),
        directory: Directory::Real(directory),
        retired: Arc::new(std::sync::atomic::AtomicBool::default()),
        backlog_allowed: Arc::new(std::sync::atomic::AtomicBool::new(true)),
        owner: Some(owner),
    };
    Ok(bound)
}

impl Endpoint for BoundListener {
    type Error = Error;

    fn ownership_error() -> Error {
        directory_error(<uds_endpoint::BoundSocket as Endpoint>::ownership_error())
    }

    fn owns(&self, basename: &str) -> bool {
        match &self.listener {
            Listener::Real(socket) => socket
                .owns_checked(basename)
                .map_err(directory_error)
                .unwrap_or(false),
            #[cfg(test)]
            Listener::Sim(socket) => socket.owns(basename),
        }
    }

    fn absent(&self, basename: &str) -> bool {
        match &self.listener {
            Listener::Real(socket) => socket
                .absent_checked(basename)
                .map_err(directory_error)
                .unwrap_or(false),
            #[cfg(test)]
            Listener::Sim(socket) => socket.absent(basename),
        }
    }

    fn same_directory(&self, other: &Self) -> Result<bool> {
        match (&self.listener, &other.listener) {
            (Listener::Real(first), Listener::Real(second)) => {
                first.same_directory(second).map_err(directory_error)
            }
            #[cfg(test)]
            (Listener::Sim(first), Listener::Sim(second)) => {
                first.same_directory(second).map_err(|_| Error::Io)
            }
            #[cfg(test)]
            _ => Ok(false),
        }
    }

    fn rename(&self, from: &str, to: &str, mode: Rename) -> Result<()> {
        #[cfg(test)]
        if FAIL_RENAME_AFTER.with(|remaining| match remaining.get() {
            Some(0) => {
                remaining.set(None);
                true
            }
            Some(count) => {
                remaining.set(Some(count - 1));
                false
            }
            None => false,
        }) {
            return Err(Error::Io);
        }
        match &self.listener {
            Listener::Real(socket) => socket.rename(from, to, mode).map_err(directory_error),
            #[cfg(test)]
            Listener::Sim(socket) => socket.rename(from, to, mode).map_err(|_| Error::Io),
        }
    }

    fn set_basename(&self, basename: String) {
        match &self.listener {
            Listener::Real(socket) => socket.set_basename(basename),
            #[cfg(test)]
            Listener::Sim(socket) => socket.set_basename(basename),
        }
    }

    fn owns_checked(&self, basename: &str) -> Result<bool> {
        match &self.listener {
            Listener::Real(socket) => socket.owns_checked(basename).map_err(directory_error),
            #[cfg(test)]
            Listener::Sim(_) => Ok(self.owns(basename)),
        }
    }

    fn absent_checked(&self, basename: &str) -> Result<bool> {
        match &self.listener {
            Listener::Real(socket) => socket.absent_checked(basename).map_err(directory_error),
            #[cfg(test)]
            Listener::Sim(_) => Ok(self.absent(basename)),
        }
    }

    fn validate_names(&self, temporary: &str, canonical: &str) -> Result<()> {
        match &self.listener {
            Listener::Real(socket) => socket
                .validate_names(temporary, canonical)
                .map_err(directory_error),
            #[cfg(test)]
            Listener::Sim(_)
                if temporary_name(temporary) && canonical == endpoint_layout()?.canonical() =>
            {
                Ok(())
            }
            #[cfg(test)]
            Listener::Sim(_) => Err(Self::ownership_error()),
        }
    }

    fn allocation_error() -> Error {
        Error::Overloaded
    }

    fn completed_error() -> Error {
        directory_error(<uds_endpoint::BoundSocket as Endpoint>::completed_error())
    }
}

// Persistent per-endpoint ownership. Never remove the lock inode, even on clean
// shutdown. Socket hard links are crash witnesses, not connect-failure heuristics.
fn endpoint_layout() -> Result<uds_endpoint::Layout> {
    uds_endpoint::Layout::new(
        ".racer-client.lock",
        "socket",
        ".racer-owned-",
        temporary_name,
    )
    .map_err(directory_error)
}

pub(super) struct EndpointOwner(Rc<uds_endpoint::EndpointOwner>);

impl EndpointOwner {
    #[cfg(test)]
    pub(super) fn clone_lock_for_test(&self) -> std::io::Result<File> {
        self.0.clone_lock_for_test()
    }

    pub(super) fn acquire(directory: &File) -> Result<Self> {
        uds_endpoint::EndpointOwner::acquire(directory, endpoint_layout()?)
            .map(|owner| Self(Rc::new(owner)))
            .map_err(directory_error)
    }

    fn validate(&self, directory: &File) -> Result<()> {
        self.0.validate(directory).map_err(directory_error)
    }
}

fn temporary_name(name: &str) -> bool {
    name.len() == 39
        && name.starts_with(".racer-")
        && name[7..].bytes().all(|b| b.is_ascii_hexdigit())
}

#[cfg(test)]
thread_local! {
    pub(super) static FAIL_ACCEPT: Cell<Option<i32>> = const { Cell::new(None) };
    pub(super) static FAIL_CHMOD: Cell<bool> = const { Cell::new(false) };
    pub(super) static FAIL_RENAME_AFTER: Cell<Option<usize>> = const { Cell::new(None) };
}
