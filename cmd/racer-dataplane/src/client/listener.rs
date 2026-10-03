//! Owned per-cache Unix sockets with permissions, bounded connections, and draining.
//! Bind /run/racer/<cache name>/client/socket; mount its client directory separately
//! from the origin directory so pods receive only their authorized endpoint.
use super::{RequestParser, response::Responses};
use crate::{
    control::state::CacheDefinition,
    error::{Error, Operation, Result},
    http::{ConnectionLease, HttpIo},
    model::{CacheId, ObjectId, RequestId},
    read::{Coordinator, ReadResponse},
    runtime::{
        admission::{AdmissionExt, AdmissionPolicy},
        deadline::{Cancellation, RequestScope},
    },
};
use std::{
    cell::{Cell, RefCell},
    collections::{BTreeMap, VecDeque},
    ffi::CString,
    fs::{self, File, OpenOptions},
    os::{
        fd::{AsRawFd, FromRawFd},
        unix::{
            fs::{FileTypeExt, MetadataExt, OpenOptionsExt, PermissionsExt},
            net::UnixListener,
        },
    },
    path::{Path, PathBuf},
    rc::Rc,
    sync::Arc,
    task::{Context, Poll},
    time::Duration,
};
use uring_runtime::reactor::Descriptor;

enum Listener {
    Real(UnixListener),
    #[cfg(test)]
    Sim(uring_runtime::reactor::Descriptor),
}
impl Listener {
    fn accept(&self) -> std::io::Result<(uring_runtime::reactor::Descriptor, ())> {
        match self {
            Self::Real(listener) => listener.accept().map(|(socket, _)| (socket.into(), ())),
            #[cfg(test)]
            Self::Sim(fd) => fd
                .as_sim()
                .expect("simulated listener")
                .accept()
                .map(|fd| (fd, ())),
        }
    }
    fn set_nonblocking(&self, value: bool) -> std::io::Result<()> {
        match self {
            Self::Real(listener) => listener.set_nonblocking(value),
            #[cfg(test)]
            Self::Sim(_) => Ok(()),
        }
    }
}
enum Directory {
    Real(File),
    #[cfg(test)]
    Sim {
        sim: uring_runtime::reactor::simulation::Simulation,
        path: PathBuf,
    },
}
impl AsRawFd for Directory {
    fn as_raw_fd(&self) -> std::os::fd::RawFd {
        match self {
            Self::Real(file) => file.as_raw_fd(),
            #[cfg(test)]
            Self::Sim { .. } => panic!("simulated directory reached host syscall"),
        }
    }
}
pub(super) fn file_path(file: &File) -> PathBuf {
    PathBuf::from(format!("/proc/self/fd/{}", file.as_raw_fd()))
}
impl Directory {
    fn anchor(&self) -> PathBuf {
        match self {
            Self::Real(file) => file_path(file),
            #[cfg(test)]
            Self::Sim { path, .. } => path.clone(),
        }
    }
}

pub(super) struct BoundListener {
    definition: CacheDefinition,
    listener: Listener,
    directory: Directory,
    device: u64,
    inode: u64,
    retired: Arc<std::sync::atomic::AtomicBool>,
    basename: RefCell<String>,
    witness: Option<String>,
    pub(super) owner: Option<Rc<EndpointOwner>>,
}

impl Drop for BoundListener {
    fn drop(&mut self) {
        self.retired
            .store(true, std::sync::atomic::Ordering::Release);
        let path = self
            .directory
            .anchor()
            .join(self.basename.borrow().as_str());
        #[cfg(test)]
        if let Directory::Sim { sim, .. } = &self.directory {
            if sim.metadata(&path).is_ok_and(|(inode, mode)| {
                inode == self.inode && mode as u32 & libc::S_IFMT == libc::S_IFSOCK
            }) {
                let _ = sim.unlink(&path);
            }
            return;
        }
        if let Ok(metadata) = fs::symlink_metadata(&path) {
            if metadata.file_type().is_socket()
                && metadata.dev() == self.device
                && metadata.ino() == self.inode
            {
                if fs::remove_file(path).is_err() {
                    // Keep the witness if pathname cleanup failed.
                    return;
                }
            }
        }
        if let Some(witness) = &self.witness {
            let path = self.directory.anchor().join(witness);
            if fs::symlink_metadata(&path).is_ok_and(|m| {
                m.file_type().is_socket() && m.dev() == self.device && m.ino() == self.inode
            }) {
                let _ = fs::remove_file(path);
            }
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
    ingress: Option<Arc<crate::runtime::ingress::Ingress>>,
    pub(super) metrics: crate::telemetry::metrics::Metrics,
    pub(super) reads: Rc<Coordinator>,
    pub(super) parser: RequestParser,
    pub(super) responses: Rc<Responses>,
    pub(super) io: Rc<HttpIo>,
    pub(super) admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
    pub(super) listeners: Rc<RefCell<BTreeMap<CacheId, Rc<BoundListener>>>>,
    pub(super) preparing: Rc<Cell<bool>>,
    cleanup: Rc<RefCell<VecDeque<Rc<BoundListener>>>>,
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
            metrics: crate::telemetry::metrics::Metrics::default(),
            reads,
            parser,
            responses,
            io,
            admission,
            listeners: Rc::new(RefCell::new(BTreeMap::new())),
            preparing: Rc::new(Cell::new(false)),
            cleanup: Rc::new(RefCell::new(VecDeque::new())),
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
    pub fn with_metrics(mut self, metrics: crate::telemetry::metrics::Metrics) -> Self {
        self.metrics = metrics;
        self
    }
    pub(crate) fn with_ingress(mut self, ingress: Arc<crate::runtime::ingress::Ingress>) -> Self {
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
        let cancellation = Cancellation::new()?;
        let task_cancellation = cancellation.clone();
        let task_retired = retired.clone();
        let idle = Rc::new(Cell::new(true));
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
                &*reads,
                &responses,
                &io,
                &admission,
                task_cancellation,
                task_retired,
                task_idle,
                timeout,
                &metrics,
                task_deadline,
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
            crate::control::state::validate_definitions(definitions)?;
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
                replacements: Vec::new(),
                preparing: self.preparing.clone(),
                accepting: self.accepting.clone(),
                cleanup: self.cleanup.clone(),
                committed: false,
            };
            // Finish older deferred unlinks before reusing a removed cache name.
            self.cleanup.borrow_mut().clear();
            let old = self.listeners.borrow().clone();
            // Commit must not allocate while adding deferred owners to the queue.
            self.cleanup
                .borrow_mut()
                .try_reserve(
                    old.len()
                        .saturating_mul(2)
                        .saturating_add(definitions.len()),
                )
                .map_err(|_| Error::Overloaded)?;
            let mut pending = Vec::new();
            // Bind and chmod every changed socket before touching any active pathname.
            for definition in definitions {
                scope.check()?;
                if let Some(current) = old
                    .get(&definition.id)
                    .filter(|current| current.definition == *definition)
                {
                    if !owns(current, "socket") {
                        return Err(Error::Io);
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
                let previous = old
                    .values()
                    .find(|current| current.definition.name == definition.name)
                    .cloned();
                let next = Rc::new(bind(
                    &self.root,
                    definition.clone(),
                    &temporary,
                    previous.as_deref(),
                )?);
                if let Some(previous) = &previous {
                    if !owns(previous, "socket")
                        || !same_directory(&previous.directory, &next.directory)?
                    {
                        return Err(Error::Io);
                    }
                } else if !absent(&next.directory, "socket") {
                    return Err(Error::Io);
                }
                prepared.next.insert(definition.id.clone(), next.clone());
                pending.push(Replacement {
                    next,
                    previous,
                    temporary,
                });
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
                let next = &replacement.next;
                if let Some(previous) = &replacement.previous {
                    if !owns(previous, "socket") || !owns(next, &replacement.temporary) {
                        return Err(Error::Io);
                    }
                    rename(
                        &next.directory,
                        &replacement.temporary,
                        "socket",
                        libc::RENAME_EXCHANGE,
                    )?;
                    *previous.basename.borrow_mut() = replacement.temporary.clone();
                } else {
                    rename(
                        &next.directory,
                        &replacement.temporary,
                        "socket",
                        libc::RENAME_NOREPLACE,
                    )?;
                }
                *next.basename.borrow_mut() = "socket".into();
                prepared.replacements.push(replacement);
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
        if budget != 0 {
            if let Some(listener) = self.cleanup.borrow_mut().pop_front() {
                drop(listener);
                cleaned = 1;
            }
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
            .limit(crate::model::ResourceClass::IngressConnection);
        // Reserve some acceptance opportunity even when all active readers wait.
        let attempts = budget.saturating_sub(worked).min(count);
        for _ in 0..attempts {
            if self
                .admission
                .used(crate::model::ResourceClass::IngressConnection)
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
                            crate::runtime::ingress::Kind::Client(
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
                Err(_) => return Err(Error::Io),
            }
            worked += 1;
        }
        Ok(worked + cleaned)
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
                if self.active.borrow().is_empty() {
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
    metrics: &crate::telemetry::metrics::Metrics,
    deadline: Rc<Cell<std::time::Instant>>,
    #[cfg(test)] read_scopes: &RefCell<Vec<RequestScope>>,
) -> Result<()> {
    loop {
        if retired.load(std::sync::atomic::Ordering::Acquire) {
            return Ok(());
        }
        idle.set(true);
        // Parsed headers and opaque context outlive HTTP staging. Charge their
        // bounded representation until the read and every stream slice complete.
        let _context = admission.reserve(
            Some(cache),
            crate::model::ResourceClass::RequestContext,
            super::MAX_HEAD_BYTES,
        )?;
        let idle_scope = new_scope(timeout, cancellation.clone())?;
        deadline.set(idle_scope.deadline.0);
        let received = io
            .receive_request_head_limited(connection, &idle_scope, parser.header_limit())
            .await?;
        connection = received.connection;
        if retired.load(std::sync::atomic::Ordering::Acquire) {
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
        if !connection.is_reusable() {
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

/// Validate a completed read before committing success headers. Keep this boundary
/// independent of acquisition so canceled or inconsistent successes fail closed.
#[allow(clippy::too_many_arguments)]
pub(super) async fn handle_read_result(
    connection: ConnectionLease,
    kind: &super::ReadKind,
    object: &ObjectId,
    read_result: Result<ReadResponse>,
    responses: &Responses,
    admission: &flow_control::Quotas<AdmissionPolicy>,
    scope: &RequestScope,
    observation: &mut crate::telemetry::metrics::RequestMetrics,
    timeout: Duration,
) -> Result<Option<ConnectionLease>> {
    let response = match read_result {
        Ok(response) => response,
        Err(error) => {
            admission.observer().record(
                crate::telemetry::failures::Failure::new(
                    crate::telemetry::failures::Stage::ClientRead,
                    error,
                )
                .request(scope),
            );
            let error = if error == Error::NotFound && kind.pin().is_some() {
                Error::VersionUnavailable
            } else {
                error
            };
            observation.fail(error);
            responses.send_error(connection, error, scope).await?;
            return Ok(None);
        }
    };
    if let Err(error) = scope.check() {
        observation.fail(error);
        responses.send_error(connection, error, scope).await?;
        return Ok(None);
    }
    if &response.metadata.version.object != object {
        responses
            .send_error(connection, Error::BadGateway, scope)
            .await?;
        return Ok(None);
    }
    if let Err(error) = responses.validate(kind, &response) {
        observation.fail(error);
        responses.send_error(connection, error, scope).await?;
        return Ok(None);
    }
    let socket = connection.socket();
    let mut send = if matches!(kind, super::ReadKind::Subscription { .. }) {
        responses.send_subscription(connection, response, scope, observation, timeout)
    } else {
        responses.send_observed(connection, response, scope, observation)
    };
    let result = std::future::poll_fn(|cx| {
        if socket.peer_disconnected() {
            let _ = scope.cancel();
        }
        send.as_mut().poll(cx)
    })
    .await;
    drop(send);
    drop(socket);
    match result {
        Ok(connection) => Ok(Some(connection)),
        Err(error) => {
            observation.fail(error);
            Err(error)
        }
    }
}

pub(super) fn new_scope(timeout: Duration, cancellation: Cancellation) -> Result<RequestScope> {
    let mut id = [0; 16];
    uring_runtime::environment::fill_random(&mut id).map_err(|_| Error::Unavailable)?;
    Ok(RequestScope {
        body_deadlines: None,
        request: RequestId(id),
        deadline: crate::runtime::deadline::Deadline(uring_runtime::environment::now() + timeout),
        cancellation,
    })
}

// Level-triggered listener index. Idle turns wait on one completion-owned poll.
#[derive(Default)]
struct ReadyListeners {
    generation: u64,
    fd: Option<Rc<Descriptor>>,
    listeners: Vec<std::rc::Weak<BoundListener>>,
    ready: VecDeque<usize>,
    wait: Option<Operation<'static, u32>>,
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
            self.wait.take();
            self.ready.clear();
            self.listeners = owner
                .listeners
                .borrow()
                .values()
                .map(Rc::downgrade)
                .collect();
            // SAFETY: epoll_create1 returns a uniquely owned descriptor.
            let fd = unsafe { libc::epoll_create1(libc::EPOLL_CLOEXEC) };
            if fd < 0 {
                return Err(Error::Io);
            }
            let fd = Rc::new(unsafe { Descriptor::from_raw_fd(fd) });
            for (index, listener) in self.listeners.iter().enumerate() {
                let listener = listener.upgrade().ok_or(Error::Unavailable)?;
                let socket = match &listener.listener {
                    Listener::Real(socket) => socket,
                    #[cfg(test)]
                    _ => return Err(Error::InvalidConfiguration),
                };
                let mut event = libc::epoll_event {
                    events: libc::EPOLLIN as u32,
                    u64: index as u64,
                };
                // SAFETY: both descriptors and the initialized event live through ctl.
                if unsafe {
                    libc::epoll_ctl(
                        fd.as_raw_fd(),
                        libc::EPOLL_CTL_ADD,
                        socket.as_raw_fd(),
                        &mut event,
                    )
                } < 0
                {
                    return Err(Error::Io);
                }
            }
            self.fd = Some(fd);
            self.scope = Some(new_scope(
                Duration::from_secs(365 * 24 * 3600),
                Cancellation::new()?,
            )?);
            self.generation = owner.generation.get();
        }
        if let Some(index) = self.ready.pop_front() {
            return Ok(self.listeners[index].upgrade());
        }
        if let Some(wait) = &mut self.wait {
            match wait.as_mut().poll(cx) {
                Poll::Pending => return Ok(None),
                Poll::Ready(result) => {
                    result?;
                    self.wait.take();
                }
            }
        }
        let Some(fd) = &self.fd else {
            return Ok(None);
        };
        let mut events = [libc::epoll_event { events: 0, u64: 0 }; 64];
        // SAFETY: events is writable for the bounded requested event count.
        let count = unsafe {
            libc::epoll_wait(
                fd.as_raw_fd(),
                events.as_mut_ptr(),
                budget.clamp(1, 64) as i32,
                0,
            )
        };
        if count < 0 {
            return Err(Error::Io);
        }
        self.ready.extend(
            events[..count as usize]
                .iter()
                .map(|event| event.u64 as usize),
        );
        if let Some(index) = self.ready.pop_front() {
            return Ok(self.listeners[index].upgrade());
        }
        let reactor = owner.io.reactor().clone();
        let fd = fd.clone();
        let scope = self.scope.as_ref().ok_or(Error::Internal)?.clone();
        self.wait = Some(Box::pin(async move {
            crate::runtime::retry_listener(&scope, || {
                reactor.readiness_with_lease(fd.clone(), libc::POLLIN as u32, fd.clone(), &scope)
            })
            .await
        }));
        if let Some(wait) = &mut self.wait {
            if let Poll::Ready(result) = wait.as_mut().poll(cx) {
                result?;
                self.wait.take();
                cx.waker().wake_by_ref();
            }
        }
        Ok(None)
    }
}

// Worker-local listener preparation. Filesystem publication precedes control
// publication; no new listener is accepted until the infallible memory swap.
struct Replacement {
    next: Rc<BoundListener>,
    previous: Option<Rc<BoundListener>>,
    temporary: String,
}

/// Owns prepared paths and sockets. Keep this on the listener's worker. Drop
/// rolls back; commit performs no filesystem or network operations.
pub struct PreparedListeners {
    generation: Rc<Cell<u64>>,
    definitions: Vec<CacheDefinition>,
    target: Rc<RefCell<BTreeMap<CacheId, Rc<BoundListener>>>>,
    next: BTreeMap<CacheId, Rc<BoundListener>>,
    replacements: Vec<Replacement>,
    preparing: Rc<Cell<bool>>,
    accepting: Rc<Cell<bool>>,
    cleanup: Rc<RefCell<VecDeque<Rc<BoundListener>>>>,
    committed: bool,
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
        if self.accepting.get() {
            let previous = std::mem::replace(&mut *target, std::mem::take(&mut self.next));
            for (id, listener) in previous {
                if !target
                    .get(&id)
                    .is_some_and(|next| Rc::ptr_eq(next, &listener))
                {
                    listener
                        .retired
                        .store(true, std::sync::atomic::Ordering::Release);
                    cleanup.push_back(listener);
                }
            }
        } else {
            // Shutdown may overtake preparation, but must never revive admission.
            for (_, listener) in std::mem::take(&mut self.next) {
                listener
                    .retired
                    .store(true, std::sync::atomic::Ordering::Release);
                cleanup.push_back(listener);
            }
        }
        for replacement in &self.replacements {
            if let Some(previous) = &replacement.previous {
                cleanup.push_back(previous.clone());
            }
        }
        self.committed = true;
    }
}

impl crate::control::state::CacheTransition for PreparedListeners {
    fn commit(self: Box<Self>) {
        (*self).commit();
    }
}

impl Drop for PreparedListeners {
    fn drop(&mut self) {
        if !self.committed {
            for replacement in self.replacements.iter().rev() {
                let next = &replacement.next;
                if let Some(previous) = &replacement.previous {
                    // Never exchange or unlink a foreign replacement inode. Under
                    // exclusive directory ownership these checks also make rollback
                    // independent of the caller's expired/canceled request scope.
                    if owns(next, "socket") && owns(previous, &replacement.temporary) {
                        if rename(
                            &next.directory,
                            "socket",
                            &replacement.temporary,
                            libc::RENAME_EXCHANGE,
                        )
                        .is_ok()
                        {
                            *next.basename.borrow_mut() = replacement.temporary.clone();
                            *previous.basename.borrow_mut() = "socket".into();
                        }
                    } else if absent(&next.directory, "socket")
                        && owns(previous, &replacement.temporary)
                    {
                        if rename(
                            &next.directory,
                            &replacement.temporary,
                            "socket",
                            libc::RENAME_NOREPLACE,
                        )
                        .is_ok()
                        {
                            *previous.basename.borrow_mut() = "socket".into();
                        }
                    }
                }
            }
        }
        self.preparing.set(false);
    }
}

pub(super) fn open_directory(path: &Path) -> Result<File> {
    // Open every component with O_NOFOLLOW, including /run/racer's ancestors.
    let mut directory = OpenOptions::new()
        .read(true)
        .custom_flags(libc::O_DIRECTORY | libc::O_NOFOLLOW | libc::O_CLOEXEC)
        .open("/")
        .map_err(|_| Error::Io)?;
    for component in path.components() {
        match component {
            std::path::Component::RootDir => {}
            std::path::Component::Normal(name) => {
                directory = child_directory(&directory, name.as_encoded_bytes())?;
            }
            _ => return Err(Error::InvalidConfiguration),
        }
    }
    Ok(directory)
}

pub(super) fn child_directory(parent: &File, name: &[u8]) -> Result<File> {
    let name = CString::new(name).map_err(|_| Error::InvalidConfiguration)?;
    // SAFETY: C strings are terminated; descriptors remain owned for each syscall.
    unsafe {
        let flags = libc::O_RDONLY | libc::O_DIRECTORY | libc::O_NOFOLLOW | libc::O_CLOEXEC;
        let mut fd = libc::openat(parent.as_raw_fd(), name.as_ptr(), flags);
        if fd < 0 && std::io::Error::last_os_error().kind() == std::io::ErrorKind::NotFound {
            if libc::mkdirat(parent.as_raw_fd(), name.as_ptr(), 0o755) != 0
                && std::io::Error::last_os_error().kind() != std::io::ErrorKind::AlreadyExists
            {
                return Err(Error::Io);
            }
            fd = libc::openat(parent.as_raw_fd(), name.as_ptr(), flags);
        }
        if fd < 0 {
            return Err(Error::Io);
        }
        Ok(File::from_raw_fd(fd))
    }
}

pub(super) fn prepare_client_directory(directory: &File) -> Result<()> {
    let before = directory.metadata().map_err(|_| Error::Io)?;
    // Only harden directories we own, even when running with CAP_FOWNER.
    // SAFETY: geteuid has no preconditions.
    if !before.is_dir() || before.uid() != unsafe { libc::geteuid() } {
        return Err(Error::Io);
    }
    let mode = before.mode() & 0o7777 & !0o022;
    if before.mode() & 0o022 != 0 {
        // The O_NOFOLLOW directory descriptor pins the inode across path swaps.
        // Never chmod ancestors or broaden existing read/search permissions.
        // SAFETY: directory owns the descriptor for the duration of fchmod.
        if unsafe { libc::fchmod(directory.as_raw_fd(), mode) } != 0 {
            return Err(Error::Io);
        }
    }
    let after = directory.metadata().map_err(|_| Error::Io)?;
    if !after.is_dir()
        || after.uid() != before.uid()
        || after.gid() != before.gid()
        || !same_inode(&before, &after)
        || after.mode() & 0o7777 != mode
    {
        return Err(Error::Io);
    }
    Ok(())
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
        sim.create_dir_all(&directory).map_err(|_| Error::Io)?;
        let path = directory.join(basename);
        let listener = sim
            .listen(uring_runtime::reactor::SocketAddress::Unix(path.clone()))
            .map_err(|_| Error::Io)?;
        let (inode, _) = sim.metadata(&path).map_err(|_| Error::Io)?;
        let bound = BoundListener {
            definition,
            listener: Listener::Sim(listener),
            directory: Directory::Sim {
                sim,
                path: directory,
            },
            device: 1,
            inode,
            retired: Arc::new(std::sync::atomic::AtomicBool::default()),
            basename: RefCell::new(basename.into()),
            witness: None,
            owner: None,
        };
        allow_socket_access(&bound.directory, bound.device, bound.inode, basename)?;
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
    let path = file_path(&directory).join(basename);
    // Bind the witness first, so every crash after bind leaves recoverable proof.
    let witness = format!(".racer-owned-{basename}");
    let witness_path = file_path(&directory).join(&witness);
    let listener = UnixListener::bind(&witness_path).map_err(|_| Error::Io)?;
    let metadata = fs::symlink_metadata(&witness_path).map_err(|_| Error::Io)?;
    let bound = BoundListener {
        definition,
        listener: Listener::Real(listener),
        directory: Directory::Real(directory),
        device: metadata.dev(),
        inode: metadata.ino(),
        retired: Arc::new(std::sync::atomic::AtomicBool::default()),
        basename: RefCell::new(basename.into()),
        witness: Some(witness),
        owner: Some(owner),
    };
    fs::hard_link(&witness_path, &path).map_err(|_| Error::Io)?;
    bound
        .listener
        .set_nonblocking(true)
        .map_err(|_| Error::Io)?;
    allow_socket_access(&bound.directory, bound.device, bound.inode, basename)?;
    Ok(bound)
}

fn allow_socket_access(
    directory: &Directory,
    device: u64,
    inode: u64,
    basename: &str,
) -> Result<()> {
    #[cfg(test)]
    if let Directory::Sim { sim, path } = directory {
        let path = path.join(basename);
        let (actual, kind) = sim.metadata(&path).map_err(|_| Error::Io)?;
        if device != 1 || actual != inode || kind as u32 & libc::S_IFMT != libc::S_IFSOCK {
            return Err(Error::Io);
        }
        if FAIL_CHMOD.with(|fail| fail.replace(false)) {
            return Err(Error::Io);
        }
        return sim.chmod(&path, 0o666).map_err(|_| Error::Io);
    }
    // Pin the final inode too: a replacement symlink must not redirect chmod.
    let socket = OpenOptions::new()
        .read(true)
        .custom_flags(libc::O_PATH | libc::O_NOFOLLOW | libc::O_CLOEXEC)
        .open(directory.anchor().join(basename))
        .map_err(|_| Error::Io)?;
    let metadata = socket.metadata().map_err(|_| Error::Io)?;
    if metadata.dev() != device || metadata.ino() != inode || !metadata.file_type().is_socket() {
        return Err(Error::Io);
    }
    #[cfg(test)]
    if FAIL_CHMOD.with(|fail| fail.replace(false)) {
        return Err(Error::Io);
    }
    // Pod volume mounts control access; every UID/GID with the mount can connect.
    // Apply explicitly so the process umask cannot restrict client access.
    fs::set_permissions(file_path(&socket), fs::Permissions::from_mode(0o666))
        .map_err(|_| Error::Io)
}

fn owns(listener: &BoundListener, basename: &str) -> bool {
    #[cfg(test)]
    if let Directory::Sim { sim, path } = &listener.directory {
        return sim
            .metadata(&path.join(basename))
            .is_ok_and(|(inode, mode)| {
                inode == listener.inode && mode as u32 & libc::S_IFMT == libc::S_IFSOCK
            });
    }
    fs::symlink_metadata(listener.directory.anchor().join(basename)).is_ok_and(|metadata| {
        metadata.file_type().is_socket()
            && metadata.dev() == listener.device
            && metadata.ino() == listener.inode
    })
}

fn absent(directory: &Directory, basename: &str) -> bool {
    #[cfg(test)]
    if let Directory::Sim { sim, path } = directory {
        return sim
            .metadata(&path.join(basename))
            .is_err_and(|e| e.kind() == std::io::ErrorKind::NotFound);
    }
    fs::symlink_metadata(directory.anchor().join(basename))
        .is_err_and(|error| error.kind() == std::io::ErrorKind::NotFound)
}

fn same_directory(first: &Directory, second: &Directory) -> Result<bool> {
    match (first, second) {
        (Directory::Real(first), Directory::Real(second)) => {
            let first = first.metadata().map_err(|_| Error::Io)?;
            let second = second.metadata().map_err(|_| Error::Io)?;
            Ok(first.dev() == second.dev() && first.ino() == second.ino())
        }
        #[cfg(test)]
        (Directory::Sim { sim, path: first }, Directory::Sim { path: second, .. }) => {
            Ok(sim.metadata(first).map_err(|_| Error::Io)?.0
                == sim.metadata(second).map_err(|_| Error::Io)?.0)
        }
        #[cfg(test)]
        _ => Ok(false),
    }
}

fn rename(directory: &Directory, from: &str, to: &str, flags: u32) -> Result<()> {
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
    #[cfg(test)]
    if let Directory::Sim { sim, path } = directory {
        return sim
            .rename(&path.join(from), &path.join(to), flags)
            .map_err(|_| Error::Io);
    }
    let from = CString::new(from).map_err(|_| Error::InvalidConfiguration)?;
    let to = CString::new(to).map_err(|_| Error::InvalidConfiguration)?;
    // SAFETY: the owned directory pins both names; strings are NUL terminated.
    if unsafe {
        libc::renameat2(
            directory.as_raw_fd(),
            from.as_ptr(),
            directory.as_raw_fd(),
            to.as_ptr(),
            flags,
        )
    } != 0
    {
        return Err(Error::Io);
    }
    Ok(())
}

// Persistent per-endpoint ownership. Never remove the lock inode, even on clean
// shutdown. Socket hard links are crash witnesses, not connect-failure heuristics.
const LOCK: &str = ".racer-client.lock";
const WITNESS: &str = ".racer-owned-";

pub(super) struct EndpointOwner {
    pub(super) lock: File,
    directory: File,
}

impl Drop for EndpointOwner {
    fn drop(&mut self) {
        // A concurrent process spawn can inherit this open file description until
        // exec closes CLOEXEC descriptors. Explicitly release our ownership rather
        // than waiting for that unrelated child to close its inherited reference.
        // BoundListener unlinks its owned paths before dropping this last owner.
        unsafe { libc::flock(self.lock.as_raw_fd(), libc::LOCK_UN) };
    }
}

impl EndpointOwner {
    pub(super) fn acquire(directory: &File) -> Result<Self> {
        let metadata = directory.metadata().map_err(|_| Error::Io)?;
        // SAFETY: geteuid has no preconditions.
        if metadata.uid() != unsafe { libc::geteuid() } || metadata.mode() & 0o022 != 0 {
            return Err(Error::Io);
        }
        let lock = OpenOptions::new()
            .read(true)
            .write(true)
            .create(true)
            .truncate(false)
            .mode(0o600)
            .custom_flags(libc::O_NOFOLLOW | libc::O_CLOEXEC | libc::O_NONBLOCK)
            .open(file_path(directory).join(LOCK))
            .map_err(|_| Error::Io)?;
        let metadata = lock.metadata().map_err(|_| Error::Io)?;
        if !metadata.is_file()
            || metadata.nlink() != 1
            || metadata.uid() != unsafe { libc::geteuid() }
            || metadata.mode() & 0o077 != 0
        {
            return Err(Error::Io);
        }
        // SAFETY: lock owns a valid descriptor. Nonblocking flock covers the entire
        // listener lifetime and is released by the kernel on process death.
        if unsafe { libc::flock(lock.as_raw_fd(), libc::LOCK_EX | libc::LOCK_NB) } != 0 {
            return Err(Error::Io);
        }
        let owner = Self {
            lock,
            directory: directory.try_clone().map_err(|_| Error::Io)?,
        };
        owner.validate(directory)?;
        owner.recover()?;
        Ok(owner)
    }

    fn validate(&self, directory: &File) -> Result<()> {
        let first = self.directory.metadata().map_err(|_| Error::Io)?;
        let second = directory.metadata().map_err(|_| Error::Io)?;
        let lock = self.lock.metadata().map_err(|_| Error::Io)?;
        let current =
            fs::symlink_metadata(file_path(directory).join(LOCK)).map_err(|_| Error::Io)?;
        if first.dev() != second.dev()
            || first.ino() != second.ino()
            || !current.is_file()
            || lock.dev() != current.dev()
            || lock.ino() != current.ino()
            || current.nlink() != 1
        {
            return Err(Error::Io);
        }
        Ok(())
    }

    fn recover(&self) -> Result<()> {
        let directory = file_path(&self.directory);
        let mut witnesses = Vec::new();
        let mut temporary_paths = Vec::new();
        for entry in fs::read_dir(&directory).map_err(|_| Error::Io)? {
            let entry = entry.map_err(|_| Error::Io)?;
            let name = entry.file_name();
            if name.to_str().is_some_and(temporary_name) {
                temporary_paths.push(entry.path());
                continue;
            }
            let Some(name) = name.to_str().filter(|name| name.starts_with(WITNESS)) else {
                continue;
            };
            let temporary = &name[WITNESS.len()..];
            if !temporary_name(temporary) {
                return Err(Error::Io);
            }
            let metadata = fs::symlink_metadata(entry.path()).map_err(|_| Error::Io)?;
            if !metadata.file_type().is_socket() {
                return Err(Error::Io);
            }
            witnesses.push((name.to_owned(), metadata));
        }
        let socket = directory.join("socket");
        let canonical = match fs::symlink_metadata(&socket) {
            Ok(metadata) => Some(metadata),
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => None,
            Err(_) => return Err(Error::Io),
        };
        if canonical.as_ref().is_some_and(|canonical| {
            !canonical.file_type().is_socket()
                || !witnesses.iter().any(|(_, m)| same_inode(canonical, m))
        }) {
            return Err(Error::Io);
        }
        // Validate every witness before touching any path. A noncooperating live
        // listener (including an inherited FD) remains protected without its lock.
        for (name, _) in &witnesses {
            refused(&directory.join(name))?;
        }
        for path in &temporary_paths {
            let current = fs::symlink_metadata(path).map_err(|_| Error::Io)?;
            if !current.file_type().is_socket()
                || !witnesses.iter().any(|(_, m)| same_inode(&current, m))
            {
                return Err(Error::Io);
            }
        }
        if canonical.is_some() {
            fs::remove_file(socket).map_err(|_| Error::Io)?;
        }
        for path in temporary_paths {
            fs::remove_file(path).map_err(|_| Error::Io)?;
        }
        for (name, _) in witnesses {
            fs::remove_file(directory.join(name)).map_err(|_| Error::Io)?;
        }
        Ok(())
    }
}

fn temporary_name(name: &str) -> bool {
    name.len() == 39
        && name.starts_with(".racer-")
        && name[7..].bytes().all(|b| b.is_ascii_hexdigit())
}

pub(super) fn same_inode(first: &fs::Metadata, second: &fs::Metadata) -> bool {
    first.dev() == second.dev() && first.ino() == second.ino()
}

fn refused(path: &Path) -> Result<()> {
    use std::os::unix::ffi::OsStrExt;
    let bytes = path.as_os_str().as_bytes();
    // SAFETY: zero is a valid initial representation for sockaddr_un.
    let mut address: libc::sockaddr_un = unsafe { std::mem::zeroed() };
    if bytes.len() >= address.sun_path.len() {
        return Err(Error::Io);
    }
    address.sun_family = libc::AF_UNIX as _;
    for (target, source) in address.sun_path.iter_mut().zip(bytes) {
        *target = *source as _;
    }
    // SAFETY: socket has no pointer arguments; File owns the returned descriptor.
    let fd = unsafe {
        libc::socket(
            libc::AF_UNIX,
            libc::SOCK_STREAM | libc::SOCK_NONBLOCK | libc::SOCK_CLOEXEC,
            0,
        )
    };
    if fd < 0 {
        return Err(Error::Io);
    }
    let socket = unsafe { File::from_raw_fd(fd) };
    // SAFETY: address is initialized and its full size is supplied.
    let result = unsafe {
        libc::connect(
            socket.as_raw_fd(),
            (&address as *const libc::sockaddr_un).cast(),
            std::mem::size_of_val(&address) as _,
        )
    };
    if result == -1 && std::io::Error::last_os_error().raw_os_error() == Some(libc::ECONNREFUSED) {
        Ok(())
    } else {
        Err(Error::Io)
    }
}

#[cfg(test)]
thread_local! {
    pub(super) static FAIL_CHMOD: Cell<bool> = const { Cell::new(false) };
    pub(super) static FAIL_RENAME_AFTER: Cell<Option<usize>> = const { Cell::new(None) };
}
