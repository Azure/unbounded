//! Owned per-cache Unix sockets with permissions, bounded connections, and draining.
//! Bind /run/racer/<cache name>/client/socket; mount its client directory separately
//! from the origin directory so pods receive only their authorized endpoint.
use super::{request::RequestParser, response::Responses};
use crate::{
    control::caches::CacheDefinition,
    error::{Error, Operation, Result},
    http::{io::HttpIo, pool::ConnectionLease},
    model::identity::{CacheId, RequestId},
    read::serve::ReadService,
    runtime::{
        admission::Admission,
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
    task::{Context, Poll},
    time::{Duration, Instant},
};

#[path = "transition.rs"]
mod transition;
pub use transition::PreparedListeners;

struct BoundListener {
    definition: CacheDefinition,
    listener: UnixListener,
    directory: File,
    device: u64,
    inode: u64,
    retired: Rc<Cell<bool>>,
    basename: RefCell<String>,
}

impl Drop for BoundListener {
    fn drop(&mut self) {
        self.retired.set(true);
        let path = anchored(&self.directory).join(self.basename.borrow().as_str());
        if let Ok(metadata) = fs::symlink_metadata(&path) {
            if metadata.file_type().is_socket()
                && metadata.dev() == self.device
                && metadata.ino() == self.inode
            {
                let _ = fs::remove_file(path);
            }
        }
    }
}

struct Active {
    cache: CacheId,
    retired: Rc<Cell<bool>>,
    idle: Rc<Cell<bool>>,
    cancellation: Cancellation,
    operation: Operation<'static, ()>,
}

pub struct ClientListeners {
    reads: Rc<dyn ReadService>,
    parser: RequestParser,
    responses: Rc<Responses>,
    io: Rc<HttpIo>,
    admission: Rc<Admission>,
    listeners: Rc<RefCell<BTreeMap<CacheId, Rc<BoundListener>>>>,
    preparing: Rc<Cell<bool>>,
    cleanup: Rc<RefCell<VecDeque<Rc<BoundListener>>>>,
    active: RefCell<VecDeque<Active>>,
    accepting: Rc<Cell<bool>>,
    accept_cursor: Cell<usize>,
    accept_turn: Cell<bool>,
    root: PathBuf,
    request_timeout: Duration,
}
impl ClientListeners {
    pub fn new(
        reads: Rc<dyn ReadService>,
        parser: RequestParser,
        responses: Rc<Responses>,
        io: Rc<HttpIo>,
        admission: Rc<Admission>,
    ) -> Self {
        Self {
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
            accept_cursor: Cell::new(0),
            accept_turn: Cell::new(true),
            root: PathBuf::from("/run/racer"),
            request_timeout: Duration::from_secs(30),
        }
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
        caches: &'a [CacheDefinition],
        scope: &'a RequestScope,
    ) -> Operation<'a, PreparedListeners> {
        transition::prepare(self, caches, scope)
    }
    /// Called by the owning I/O worker alongside reactor completion polling.
    /// Work is bounded across accepts and futures; no background executor exists.
    pub fn poll_budgeted(&self, budget: usize) -> Result<usize> {
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
        let waker = futures::task::noop_waker();
        let mut context = Context::from_waker(&waker);
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
            if active.retired.get() && active.idle.get() {
                let _ = active.cancellation.cancel();
            }
            match active.operation.as_mut().poll(&mut context) {
                Poll::Pending => self.active.borrow_mut().push_back(active),
                Poll::Ready(_) => {}
            }
            worked += 1;
        }
        if !self.accepting.get() {
            return Ok(worked + cleaned);
        }
        let listeners = self.listeners.borrow();
        let count = listeners.len();
        if count == 0 {
            return Ok(worked + cleaned);
        }
        let maximum = self.admission.limits().client_connections.get();
        // Reserve some acceptance opportunity even when all active readers wait.
        let attempts = budget.saturating_sub(worked).min(count);
        for step in 0..attempts {
            if self.active.borrow().len() >= maximum {
                break;
            }
            let index = (self.accept_cursor.get() + step) % count;
            let (_, listener) = listeners.iter().nth(index).ok_or(Error::Internal)?;
            match listener.listener.accept() {
                Ok((socket, _)) => {
                    let connection =
                        match ConnectionLease::from_accepted(socket.into(), &self.admission) {
                            Ok(connection) => connection,
                            Err(Error::Overloaded) => break,
                            Err(error) => return Err(error),
                        };
                    let cache = listener.definition.id.clone();
                    let cancellation = Cancellation::new()?;
                    let task_cancellation = cancellation.clone();
                    let retired = listener.retired.clone();
                    let task_retired = retired.clone();
                    let idle = Rc::new(Cell::new(true));
                    let task_idle = idle.clone();
                    let timeout = self.request_timeout;
                    let parser = self.parser.clone();
                    let reads = self.reads.clone();
                    let responses = self.responses.clone();
                    let io = self.io.clone();
                    let admission = self.admission.clone();
                    let task_cache = cache.clone();
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
                        )
                        .await
                    });
                    self.active.borrow_mut().push_back(Active {
                        cache,
                        retired,
                        idle,
                        cancellation,
                        operation,
                    });
                }
                Err(error) if error.kind() == std::io::ErrorKind::WouldBlock => {}
                Err(error) if error.kind() == std::io::ErrorKind::Interrupted => {}
                Err(_) => return Err(Error::Io),
            }
            worked += 1;
        }
        self.accept_cursor
            .set((self.accept_cursor.get() + attempts) % count);
        Ok(worked + cleaned)
    }

    pub fn active_connections(&self) -> usize {
        self.active.borrow().len()
    }

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
            listener.retired.set(true);
            self.cleanup.borrow_mut().push_back(listener);
        }
        for active in self
            .active
            .borrow()
            .iter()
            .filter(|active| &active.cache == cache)
        {
            active.retired.set(true);
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

    pub fn drain_cache<'a>(
        &'a self,
        cache: &'a CacheId,
        scope: &'a RequestScope,
    ) -> Operation<'a, ()> {
        Box::pin(async move {
            self.stop_cache(cache);
            std::future::poll_fn(|cx| {
                if let Err(error) = scope.check() {
                    let _ = self.cancel_cache(cache);
                    return Poll::Ready(Err(error));
                }
                if let Err(error) = self.poll_budgeted(64) {
                    return Poll::Ready(Err(error));
                }
                if self.active_connections_for(cache) == 0 {
                    Poll::Ready(Ok(()))
                } else {
                    cx.waker().wake_by_ref();
                    Poll::Pending
                }
            })
            .await
        })
    }

    pub fn stop_admission(&self) {
        self.accepting.set(false);
        for (_, listener) in std::mem::take(&mut *self.listeners.borrow_mut()) {
            listener.retired.set(true);
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
                if let Err(error) = self.poll_budgeted(64) {
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
    reads: &dyn ReadService,
    responses: &Responses,
    io: &HttpIo,
    admission: &Admission,
    cancellation: Cancellation,
    retired: Rc<Cell<bool>>,
    idle: Rc<Cell<bool>>,
    timeout: Duration,
) -> Result<()> {
    loop {
        if retired.get() {
            return Ok(());
        }
        idle.set(true);
        // Parsed headers and opaque context outlive HTTP staging. Charge their
        // bounded representation until the read and every stream slice complete.
        let _context = admission.reserve(
            Some(cache),
            crate::model::limits::ResourceClass::RequestContext,
            super::request::MAX_HEAD_BYTES,
        )?;
        let idle_scope = new_scope(timeout, cancellation.clone())?;
        let received = io
            .receive_request_head_limited(connection, &idle_scope, parser.header_limit())
            .await?;
        connection = received.connection;
        if retired.get() {
            return Ok(());
        }
        idle.set(false);
        // Idle/header admission has its own bound. A healthy pooled connection
        // gets a fresh operation budget only after each complete request head.
        let scope = new_scope(timeout, cancellation.clone())?;
        let scope = &scope;
        let request = match received.value.and_then(|head| parser.parse(cache, head)) {
            Ok(request) => request,
            Err(error) => {
                responses.send_error(connection, error, scope).await?;
                return Ok(());
            }
        };
        let kind = request.kind.clone();
        let object = request.origin.object.clone();
        let response = match reads.read(request, scope).await {
            Ok(response) => response,
            Err(error) => {
                let error = if error == Error::NotFound && kind.pin().is_some() {
                    Error::VersionUnavailable
                } else {
                    error
                };
                responses.send_error(connection, error, scope).await?;
                return Ok(());
            }
        };
        if let Err(error) = scope.check() {
            responses.send_error(connection, error, scope).await?;
            return Ok(());
        }
        if response.metadata.version.object != object {
            responses
                .send_error(connection, Error::BadGateway, scope)
                .await?;
            return Ok(());
        }
        if let Err(error) = responses.validate(&kind, &response) {
            responses.send_error(connection, error, scope).await?;
            return Ok(());
        }
        connection = responses.send(connection, response, scope).await?;
        if !connection.is_reusable() {
            return Ok(());
        }
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

fn new_scope(timeout: Duration, cancellation: Cancellation) -> Result<RequestScope> {
    let mut id = [0; 16];
    getrandom::getrandom(&mut id).map_err(|_| Error::Unavailable)?;
    Ok(RequestScope {
        request: RequestId(id),
        deadline: crate::runtime::deadline::Deadline(Instant::now() + timeout),
        cancellation,
    })
}

fn anchored(directory: &File) -> PathBuf {
    PathBuf::from(format!("/proc/self/fd/{}", directory.as_raw_fd()))
}

fn open_directory(path: &Path) -> Result<File> {
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

fn child_directory(parent: &File, name: &[u8]) -> Result<File> {
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

fn bind(root: &Path, definition: CacheDefinition, basename: &str) -> Result<BoundListener> {
    let root = open_directory(root)?;
    let cache = child_directory(&root, definition.name.as_bytes())?;
    let directory = child_directory(&cache, b"client")?;
    let path = anchored(&directory).join(basename);
    // Never unlink an existing socket, even if it appears stale: it is not ours.
    let listener = UnixListener::bind(&path).map_err(|_| Error::Io)?;
    let metadata = fs::symlink_metadata(&path).map_err(|_| Error::Io)?;
    let bound = BoundListener {
        definition,
        listener,
        directory,
        device: metadata.dev(),
        inode: metadata.ino(),
        retired: Rc::new(Cell::new(false)),
        basename: RefCell::new(basename.into()),
    };
    bound
        .listener
        .set_nonblocking(true)
        .map_err(|_| Error::Io)?;
    set_socket_mode(
        &bound.directory,
        bound.device,
        bound.inode,
        bound.definition.socket_mode,
        basename,
    )?;
    Ok(bound)
}

fn set_socket_mode(
    directory: &File,
    device: u64,
    inode: u64,
    mode: u32,
    basename: &str,
) -> Result<()> {
    // Pin the final inode too: a replacement symlink must not redirect chmod.
    let socket = OpenOptions::new()
        .read(true)
        .custom_flags(libc::O_PATH | libc::O_NOFOLLOW | libc::O_CLOEXEC)
        .open(anchored(directory).join(basename))
        .map_err(|_| Error::Io)?;
    let metadata = socket.metadata().map_err(|_| Error::Io)?;
    if metadata.dev() != device || metadata.ino() != inode || !metadata.file_type().is_socket() {
        return Err(Error::Io);
    }
    #[cfg(test)]
    if FAIL_CHMOD.with(|fail| fail.replace(false)) {
        return Err(Error::Io);
    }
    fs::set_permissions(anchored(&socket), fs::Permissions::from_mode(mode)).map_err(|_| Error::Io)
}
#[cfg(test)]
thread_local! {
    static FAIL_CHMOD: Cell<bool> = const { Cell::new(false) };
}
#[cfg(test)]
mod tests {
    use super::*;
    use crate::{
        client::request::ClientRequest,
        http::codec::Codec,
        memory::{delivery::Delivery, pipe::PipePool},
        model::{
            identity::{ObjectVersion, StrongEtag},
            limits::Limits,
            metadata::{ExpiresAt, ObjectMetadata},
        },
        read::serve::ReadResponse,
        runtime::reactor::Reactor,
    };
    use std::{
        io::{Read, Write},
        num::NonZeroUsize,
        os::unix::net::UnixStream,
        sync::atomic::{AtomicUsize, Ordering},
        time::UNIX_EPOCH,
    };

    static NEXT_ROOT: AtomicUsize = AtomicUsize::new(0);
    struct Root(PathBuf);
    impl Root {
        fn new() -> Self {
            // Test files stay inside the shared worktree, even on unprivileged runs.
            let path = Path::new(env!("CARGO_MANIFEST_DIR")).join(format!(
                ".client-test-{}-{}",
                std::process::id(),
                NEXT_ROOT.fetch_add(1, Ordering::Relaxed)
            ));
            fs::create_dir(&path).unwrap();
            Self(path)
        }
    }
    impl Drop for Root {
        fn drop(&mut self) {
            let _ = fs::remove_dir_all(&self.0);
        }
    }
    fn limits() -> Limits {
        let n = NonZeroUsize::new(64).unwrap();
        let bytes = NonZeroUsize::new(64 * 1024 * 1024).unwrap();
        Limits {
            plaintext_bytes: bytes,
            ciphertext_bytes: bytes,
            dirty_bytes: bytes,
            registered_bytes: bytes,
            request_context_bytes: bytes,
            flights: n,
            waiters_per_flight: n,
            queue_entries: n,
            connections_per_neighbor: n,
            client_connections: n,
            pipes: n,
            range_window_pages: n,
            replay_entries: n,
            header_bytes: NonZeroUsize::new(32768).unwrap(),
            route_search_work: n,
            cached_rankings: n,
            cached_paths: n,
            retained_snapshots: n,
            metadata_entries: n,
            relay_transfers: n,
        }
    }
    fn definition() -> CacheDefinition {
        CacheDefinition {
            id: CacheId("00000000-0000-4000-8000-000000000001".into()),
            name: "example".into(),
            client_socket: "/run/racer/example/client/socket".into(),
            origin_socket: "/run/racer/example/origin/socket".into(),
            socket_mode: 0o600,
        }
    }
    fn scope() -> RequestScope {
        new_scope(Duration::from_secs(5), Cancellation::new().unwrap()).unwrap()
    }
    struct Heads {
        calls: Cell<usize>,
    }
    impl ReadService for Heads {
        fn read<'a>(
            &'a self,
            request: ClientRequest,
            scope: &'a RequestScope,
        ) -> Operation<'a, ReadResponse> {
            Box::pin(async move {
                scope.check()?;
                self.calls.set(self.calls.get() + 1);
                Ok(ReadResponse {
                    metadata: ObjectMetadata {
                        version: ObjectVersion {
                            object: request.origin.object,
                            etag: StrongEtag::parse(b"\"v1\"")?,
                        },
                        length: 17,
                        expires_at: ExpiresAt(UNIX_EPOCH + Duration::from_millis(1234)),
                    },
                    range: None,
                    body: None,
                })
            })
        }
    }
    struct Fixture {
        root: Root,
        listeners: ClientListeners,
        reactor: Rc<Reactor>,
        reads: Rc<Heads>,
    }
    impl Fixture {
        fn new() -> Self {
            let root = Root::new();
            let admission = Rc::new(Admission::new(limits()));
            let reactor = Rc::new(Reactor::new(admission.clone()));
            let io = Rc::new(HttpIo::with_admission(
                reactor.clone(),
                Codec::new(32768, i64::MAX as u64),
                admission.clone(),
            ));
            let delivery = Rc::new(Delivery::new(
                Rc::new(PipePool::new(admission.clone(), reactor.clone())),
                Duration::from_secs(2),
            ));
            let reads = Rc::new(Heads {
                calls: Cell::new(0),
            });
            let responses = Rc::new(Responses::new(io.clone(), delivery));
            let mut listeners = ClientListeners::new(
                reads.clone(),
                RequestParser::new(32768),
                responses,
                io,
                admission,
            );
            listeners.root = root.0.clone();
            Self {
                root,
                listeners,
                reactor,
                reads,
            }
        }
        fn reconcile(&self, caches: &[CacheDefinition]) -> Result<()> {
            futures::executor::block_on(self.listeners.reconcile(caches, &scope()))
        }
        fn socket(&self) -> PathBuf {
            self.root.0.join("example/client/socket")
        }
        fn connect(&self) -> UnixStream {
            // /proc keeps sun_path short even in deeply nested CI worktrees.
            let directory = File::open(self.socket().parent().unwrap()).unwrap();
            let stream = UnixStream::connect(anchored(&directory).join("socket")).unwrap();
            stream.set_nonblocking(true).unwrap();
            stream
        }
        fn pump(&self, budget: usize) {
            self.listeners.poll_budgeted(budget).unwrap();
            self.reactor.poll_budgeted(64).unwrap();
        }
        fn receive(&self, socket: &mut UnixStream, eof: bool) -> Vec<u8> {
            let deadline = Instant::now() + Duration::from_secs(5);
            let mut output = Vec::new();
            loop {
                self.pump(16);
                let mut bytes = [0; 8192];
                match socket.read(&mut bytes) {
                    Ok(0) => return output,
                    Ok(n) => output.extend_from_slice(&bytes[..n]),
                    Err(error) if error.kind() == std::io::ErrorKind::WouldBlock => {}
                    Err(error) => panic!("client read failed: {error}"),
                }
                if !eof && output.windows(4).any(|part| part == b"\r\n\r\n") {
                    return output;
                }
                assert!(
                    Instant::now() < deadline,
                    "client exchange did not complete"
                );
            }
        }
    }
    fn request(method: &str, fields: &str) -> Vec<u8> {
        format!(
            "{method} /v1/objects/{} HTTP/1.1\r\nHost: racer\r\n{fields}\r\n",
            "0".repeat(64)
        )
        .into_bytes()
    }

    struct Bodies {
        admission: Rc<Admission>,
        streams: crate::read::range_stream::RangeStreams,
        late_failure: bool,
    }
    impl ReadService for Bodies {
        fn read<'a>(
            &'a self,
            request: ClientRequest,
            scope: &'a RequestScope,
        ) -> Operation<'a, ReadResponse> {
            Box::pin(async move {
                use crate::{
                    memory::{
                        page::PageResult,
                        pool::{CiphertextBytes, CiphertextPage, VerifiedBytes, VerifiedPage},
                    },
                    model::{
                        envelope::{KeyId, Nonce, PageEnvelope},
                        identity::{MembershipVersion, PageId, PageNumber},
                        limits::ResourceClass,
                        range::{ByteRange, PAGE_BYTES},
                    },
                    topology::membership::Membership,
                };
                use std::sync::Arc;
                let length = if self.late_failure { PAGE_BYTES + 1 } else { 5 };
                let metadata = ObjectMetadata {
                    version: ObjectVersion {
                        object: request.origin.object.clone(),
                        etag: StrongEtag::parse(b"\"v1\"")?,
                    },
                    length,
                    expires_at: ExpiresAt(UNIX_EPOCH + Duration::from_millis(1234)),
                };
                let requested = match request.kind {
                    super::super::request::ReadKind::Pinned { range, .. } => range,
                    _ => ByteRange::Closed {
                        first: 0,
                        last: PAGE_BYTES - 1,
                    },
                };
                let range = requested.resolve(length)?;
                let page = PageId {
                    version: metadata.version.clone(),
                    number: PageNumber(0),
                };
                let bytes = if self.late_failure {
                    vec![b'x'; PAGE_BYTES as usize]
                } else {
                    b"hello".to_vec()
                };
                let envelope = PageEnvelope {
                    page: page.clone(),
                    key_id: KeyId([0; 16]),
                    nonce: Nonce([0; 24]),
                    plaintext_length: bytes.len() as u32,
                    ciphertext_length: bytes.len() as u32 + 16,
                };
                let plaintext = VerifiedPage {
                    inner: Arc::new(VerifiedBytes {
                        page,
                        reservation: self.admission.reserve(
                            None,
                            ResourceClass::Plaintext,
                            bytes.len(),
                        )?,
                        bytes,
                    }),
                };
                let ciphertext = CiphertextPage {
                    inner: Arc::new(CiphertextBytes {
                        reservation: self.admission.reserve(
                            None,
                            ResourceClass::Ciphertext,
                            envelope.ciphertext_length as usize,
                        )?,
                        bytes: vec![0; envelope.ciphertext_length as usize],
                        envelope,
                    }),
                };
                let seed = PageResult {
                    metadata: metadata.clone(),
                    plaintext,
                    ciphertext,
                };
                let stream = self.streams.open_with_budget(
                    metadata.clone(),
                    range,
                    request.origin,
                    Arc::new(Membership::validate(MembershipVersion(1), vec![])?),
                    scope.clone(),
                    crate::read::flight::AcquisitionBudget::new(scope.deadline.0, 4, 4),
                    Some(seed),
                )?;
                Ok(ReadResponse {
                    metadata,
                    range: Some(range),
                    body: Some(stream),
                })
            })
        }
    }

    #[test]
    fn actual_uds_nonempty_range_and_late_failure_truncates() {
        use crate::{
            model::identity::WorkerId,
            read::{dispatch::WorkerDirectory, range_stream::RangeStreams},
            runtime::worker::WorkerMap,
        };
        use std::sync::Arc;
        for (late_failure, fields, expected_range, expected_length, expected_body) in [
            (
                false,
                "Range: bytes=0-16777215\r\n",
                "bytes 0-4/5",
                5,
                b"hello".as_slice(),
            ),
            (
                false,
                "If-Match: \"v1\"\r\nRange: bytes=1-3\r\n",
                "bytes 1-3/5",
                3,
                b"ell".as_slice(),
            ),
            (
                false,
                "If-Match: \"v1\"\r\nRange: bytes=2-\r\n",
                "bytes 2-4/5",
                3,
                b"llo".as_slice(),
            ),
            (
                false,
                "If-Match: \"v1\"\r\nRange: bytes=-2\r\n",
                "bytes 3-4/5",
                2,
                b"lo".as_slice(),
            ),
            (
                true,
                "If-Match: \"v1\"\r\nRange: bytes=16777214-16777216\r\n",
                "bytes 16777214-16777216/16777217",
                3,
                b"xx".as_slice(),
            ),
        ] {
            let mut fixture = Fixture::new();
            let admission = fixture.listeners.admission.clone();
            let delivery = Rc::new(Delivery::new(
                Rc::new(PipePool::new(admission.clone(), fixture.reactor.clone())),
                Duration::from_secs(2),
            ));
            let directory = Arc::new(
                WorkerDirectory::new(
                    Arc::new(WorkerMap::new(vec![WorkerId(0)]).unwrap()),
                    vec![WorkerId(0)],
                    4,
                )
                .unwrap(),
            );
            // Seed the first authenticated page; an unavailable owner for page one
            // causes a real acquisition failure only after the first slice escaped.
            fixture.listeners.reads = Rc::new(Bodies {
                admission,
                streams: RangeStreams::from_directory(directory, delivery, 1),
                late_failure,
            });
            fixture.reconcile(&[definition()]).unwrap();
            let mut socket = fixture.connect();
            socket
                .write_all(&request("GET", &format!("{fields}Connection: close\r\n")))
                .unwrap();
            let output = fixture.receive(&mut socket, true);
            let end = output
                .windows(4)
                .position(|part| part == b"\r\n\r\n")
                .unwrap()
                + 4;
            let head = std::str::from_utf8(&output[..end])
                .unwrap()
                .to_ascii_lowercase();
            assert!(head.starts_with("http/1.1 206"), "{head}");
            assert!(head.contains("content-type: application/octet-stream\r\n"));
            assert!(head.contains(&format!("content-length: {expected_length}\r\n")));
            assert!(head.contains(&format!("content-range: {expected_range}\r\n")));
            assert_eq!(&output[end..], expected_body);
        }
    }

    #[test]
    fn actual_uds_empty_bootstrap_and_cancelled_success() {
        struct Empty(bool);
        impl ReadService for Empty {
            fn read<'a>(
                &'a self,
                request: ClientRequest,
                scope: &'a RequestScope,
            ) -> Operation<'a, ReadResponse> {
                Box::pin(async move {
                    if self.0 {
                        scope.cancel()?;
                    }
                    Ok(ReadResponse {
                        metadata: ObjectMetadata {
                            version: ObjectVersion {
                                object: request.origin.object,
                                etag: StrongEtag::parse(b"\"\"")?,
                            },
                            length: 0,
                            expires_at: ExpiresAt(UNIX_EPOCH),
                        },
                        range: None,
                        body: None,
                    })
                })
            }
        }
        let mut fixture = Fixture::new();
        fixture.reconcile(&[definition()]).unwrap();
        for cancelled in [false, true] {
            fixture.listeners.reads = Rc::new(Empty(cancelled));
            let mut socket = fixture.connect();
            socket
                .write_all(&request(
                    "GET",
                    "Range: bytes=0-16777215\r\nConnection: close\r\n",
                ))
                .unwrap();
            let output = fixture.receive(&mut socket, true);
            let text = std::str::from_utf8(&output).unwrap().to_ascii_lowercase();
            let status = if cancelled { 503 } else { 200 };
            assert!(text.starts_with(&format!("http/1.1 {status}")), "{text}");
            assert!(text.contains("content-length: 0\r\n"));
            assert!(!text.contains("content-range:"));
            assert!(text.ends_with("\r\n\r\n"));
            if !cancelled {
                assert!(text.contains("etag: \"\"\r\n"));
                assert!(text.contains("racer-expires-at: 0\r\n"));
                assert!(text.contains("content-type: application/octet-stream\r\n"));
            }
        }
    }

    #[test]
    fn actual_uds_head_keepalive_removal_and_accept_fairness() {
        let fixture = Fixture::new();
        assert!(!fixture.socket().exists());
        fixture.reconcile(&[definition()]).unwrap();
        assert_eq!(
            fs::metadata(fixture.socket()).unwrap().mode() & 0o777,
            0o600
        );
        let mut idle = fixture.connect();
        fixture.pump(1);
        let mut socket = fixture.connect();
        socket
            .write_all(&request("HEAD", "If-Match: \"v1\"\r\n"))
            .unwrap();
        // A waiting idle connection must not monopolize a single-unit poll budget.
        for _ in 0..8 {
            fixture.pump(1);
        }
        assert_eq!(fixture.listeners.active_connections(), 2);
        let first = fixture.receive(&mut socket, false);
        let text = std::str::from_utf8(&first).unwrap().to_ascii_lowercase();
        assert!(text.starts_with("http/1.1 200"));
        assert!(text.contains("content-length: 17\r\n"));
        assert!(text.contains("etag: \"v1\"\r\n"));
        assert!(text.contains("racer-expires-at: 1234\r\n"));
        assert!(text.ends_with("\r\n\r\n"));
        socket.write_all(&request("HEAD", "")).unwrap();
        fixture.receive(&mut socket, false);
        assert_eq!(fixture.reads.calls.get(), 2);
        fixture.reconcile(&[]).unwrap();
        assert!(!fixture.socket().exists());
        assert!(fixture.receive(&mut socket, true).is_empty());
        assert!(fixture.receive(&mut idle, true).is_empty());
        assert_eq!(fixture.reads.calls.get(), 2);
    }

    #[test]
    fn actual_uds_errors_and_idle_drain() {
        let fixture = Fixture::new();
        fixture.reconcile(&[definition()]).unwrap();
        for (method, fields, status, extra) in [
            ("POST", "", "405", "allow: head, get\r\n"),
            (
                "GET",
                "Range: bytes=1-2\r\n",
                "400",
                "content-length: 0\r\n",
            ),
            (
                "HEAD",
                "Authorization: \r\n",
                "400",
                "content-length: 0\r\n",
            ),
        ] {
            let mut socket = fixture.connect();
            socket.write_all(&request(method, fields)).unwrap();
            let output = fixture.receive(&mut socket, true);
            let text = std::str::from_utf8(&output).unwrap().to_ascii_lowercase();
            assert!(text.starts_with(&format!("http/1.1 {status}")), "{text}");
            assert!(text.contains(extra), "{text}");
            assert!(text.ends_with("\r\n\r\n"));
        }
        assert_eq!(fixture.reads.calls.get(), 0);
        let mut idle = fixture.connect();
        fixture.pump(16);
        fixture.pump(16);
        fixture.listeners.stop_admission();
        assert!(fixture.receive(&mut idle, true).is_empty());
        futures::executor::block_on(fixture.listeners.drain(&scope())).unwrap();
        assert_eq!(fixture.listeners.active_connections(), 0);
    }

    #[test]
    fn actual_uds_rejected_raw_heads_receive_sdk_errors() {
        let fixture = Fixture::new();
        fixture.reconcile(&[definition()]).unwrap();
        let mut oversized = request("HEAD", &format!("X-Padding: {}\r\n", "x".repeat(32768)));
        // Hit the cap without sending beyond it: a full unterminated head is 431.
        oversized.truncate(32768);
        for (raw, status) in [
            (request("HEAD", "Authorization:secret\r\n"), 400),
            (request("HEAD", "Authorization:  secret\r\n"), 400),
            (
                request("HEAD", "Content-Length: 0\r\nContent-Length: 0\r\n"),
                400,
            ),
            (request("HEAD", "Transfer-Encoding: chunked\r\n"), 400),
            (oversized, 431),
            (
                request("HEAD", &format!("Racer-Metadata: {}\r\n", "x".repeat(8193))),
                431,
            ),
        ] {
            let mut socket = fixture.connect();
            socket.write_all(&raw).unwrap();
            let output = fixture.receive(&mut socket, true);
            let text = std::str::from_utf8(&output).unwrap().to_ascii_lowercase();
            assert!(text.starts_with(&format!("http/1.1 {status}")), "{text}");
            assert!(text.contains("content-length: 0\r\n"));
            assert!(text.ends_with("\r\n\r\n"));
        }
        assert_eq!(fixture.reads.calls.get(), 0);
    }

    #[test]
    fn actual_uds_configured_head_cap_counts_only_wire_bytes() {
        for limit in [512, super::super::request::MAX_HEAD_BYTES] {
            let mut fixture = Fixture::new();
            fixture.listeners.parser = RequestParser::new(limit);
            fixture.reconcile(&[definition()]).unwrap();
            for separator in ["", " ", "\t", "  "] {
                let fields = format!(
                    "Connection: close\r\nX:{separator}{}\r\n",
                    "x".repeat(
                        limit
                            - request("HEAD", &format!("Connection: close\r\nX:{separator}\r\n"))
                                .len()
                    )
                );
                let raw = request("HEAD", &fields);
                assert_eq!(raw.len(), limit);
                let mut socket = fixture.connect();
                socket.write_all(&raw).unwrap();
                let output = fixture.receive(&mut socket, true);
                assert!(output.starts_with(b"HTTP/1.1 200 "));
            }
            let admitted = fixture.reads.calls.get();
            // Exhaust the raw cap on an unterminated head. Decoded value bytes
            // alone would fit; only framing can reject this before dispatch.
            let mut raw = request("HEAD", &format!("X:{}\r\n", "x".repeat(limit)));
            raw.truncate(limit);
            let mut socket = fixture.connect();
            socket.write_all(&raw).unwrap();
            let output = fixture.receive(&mut socket, true);
            assert!(output.starts_with(b"HTTP/1.1 431 "));
            assert_eq!(fixture.reads.calls.get(), admitted);
        }
    }

    #[test]
    fn actual_uds_configured_head_limit_counts_received_bytes() {
        let mut fixture = Fixture::new();
        let limit = 512;
        fixture.listeners.parser = RequestParser::new(limit);
        fixture.reconcile(&[definition()]).unwrap();
        for separator in ["", " ", "\t", " \t"] {
            let prefix = request("HEAD", &format!("X:{separator}"));
            // Replace the request helper's final CRLF with field data and the
            // complete terminator. Unknown-field whitespace stays on the wire.
            let mut exact = prefix[..prefix.len() - 2].to_vec();
            exact.resize(limit - 4, b'x');
            exact.extend_from_slice(b"\r\n\r\n");
            let mut socket = fixture.connect();
            socket.write_all(&exact).unwrap();
            let reply = fixture.receive(&mut socket, false);
            assert!(reply.starts_with(b"HTTP/1.1 200 "));
            drop(socket);

            // This is the first limit bytes of a limit+1-byte head: the final
            // LF lies beyond the configured cap, so framing must reject it.
            exact.insert(exact.len() - 4, b'x');
            exact.truncate(limit);
            let mut socket = fixture.connect();
            socket.write_all(&exact).unwrap();
            let reply = fixture.receive(&mut socket, true);
            assert!(reply.starts_with(b"HTTP/1.1 431 "));
        }
        assert_eq!(fixture.reads.calls.get(), 4);
    }

    #[test]
    fn actual_uds_read_failures_and_immutable_result_validation() {
        struct Failing(Error);
        impl ReadService for Failing {
            fn read<'a>(
                &'a self,
                _: ClientRequest,
                scope: &'a RequestScope,
            ) -> Operation<'a, ReadResponse> {
                Box::pin(async move {
                    if self.0 == Error::Cancelled {
                        scope.cancel()?;
                    }
                    Err(self.0)
                })
            }
        }
        let mut fixture = Fixture::new();
        fixture.reconcile(&[definition()]).unwrap();
        for (error, fields, status, range) in [
            (Error::NotFound, "", 404, None),
            (Error::NotFound, "If-Match: \"v1\"\r\n", 412, None),
            (
                Error::UnsatisfiableRangeWithLength(17),
                "",
                416,
                Some("content-range: bytes */17\r\n"),
            ),
            (Error::Cancelled, "", 503, None),
            (Error::OriginRejected, "", 401, None),
            (Error::OriginForbidden, "", 403, None),
        ] {
            fixture.listeners.reads = Rc::new(Failing(error));
            let mut socket = fixture.connect();
            socket.write_all(&request("HEAD", fields)).unwrap();
            let output = fixture.receive(&mut socket, true);
            let text = std::str::from_utf8(&output).unwrap().to_ascii_lowercase();
            assert!(text.starts_with(&format!("http/1.1 {status}")), "{text}");
            assert!(text.contains("content-length: 0\r\n"));
            assert!(!text.contains("etag:") && !text.contains("racer-expires-at:"));
            if let Some(range) = range {
                assert!(text.contains(range));
            }
            assert!(text.ends_with("\r\n\r\n"));
        }
        struct WrongIdentity(bool);
        impl ReadService for WrongIdentity {
            fn read<'a>(
                &'a self,
                mut request: ClientRequest,
                _: &'a RequestScope,
            ) -> Operation<'a, ReadResponse> {
                Box::pin(async move {
                    if self.0 {
                        request.origin.object.key.0[0] ^= 1;
                    }
                    Ok(ReadResponse {
                        metadata: ObjectMetadata {
                            version: ObjectVersion {
                                object: request.origin.object,
                                etag: StrongEtag::parse(b"\"other\"")?,
                            },
                            length: 0,
                            expires_at: ExpiresAt(UNIX_EPOCH),
                        },
                        range: None,
                        body: None,
                    })
                })
            }
        }
        for wrong_object in [true, false] {
            fixture.listeners.reads = Rc::new(WrongIdentity(wrong_object));
            let mut socket = fixture.connect();
            socket
                .write_all(&request(
                    "HEAD",
                    if wrong_object {
                        ""
                    } else {
                        "If-Match: \"v1\"\r\n"
                    },
                ))
                .unwrap();
            let output = fixture.receive(&mut socket, true);
            assert!(output.starts_with(b"HTTP/1.1 502"));
        }
    }

    #[test]
    fn prepared_transition_rolls_back_bind_chmod_and_drop() {
        let fixture = Fixture::new();
        fixture.reconcile(&[definition()]).unwrap();
        let inode = fs::metadata(fixture.socket()).unwrap().ino();
        let mut changed = definition();
        changed.socket_mode = 0o660;
        let mut added = definition();
        added.id = CacheId("00000000-0000-4000-8000-000000000002".into());
        added.name = "blocked".into();
        added.client_socket = "/run/racer/blocked/client/socket".into();
        added.origin_socket = "/run/racer/blocked/origin/socket".into();
        fs::write(fixture.root.0.join("blocked"), b"foreign").unwrap();
        assert!(
            futures::executor::block_on(
                fixture
                    .listeners
                    .prepare(&[changed.clone(), added], &scope())
            )
            .is_err()
        );
        assert_eq!(fs::metadata(fixture.socket()).unwrap().ino(), inode);
        assert_eq!(
            fs::metadata(fixture.socket()).unwrap().mode() & 0o777,
            0o600
        );
        FAIL_CHMOD.with(|fail| fail.set(true));
        assert!(
            futures::executor::block_on(fixture.listeners.prepare(&[changed.clone()], &scope()))
                .is_err()
        );
        assert_eq!(fs::metadata(fixture.socket()).unwrap().ino(), inode);
        assert_eq!(
            fs::metadata(fixture.socket()).unwrap().mode() & 0o777,
            0o600
        );
        let prepared =
            futures::executor::block_on(fixture.listeners.prepare(&[changed.clone()], &scope()))
                .unwrap();
        assert_ne!(fs::metadata(fixture.socket()).unwrap().ino(), inode);
        assert_eq!(
            fs::metadata(fixture.socket()).unwrap().mode() & 0o777,
            0o660
        );
        assert!(matches!(
            futures::executor::block_on(fixture.listeners.prepare(&[], &scope())),
            Err(Error::Overloaded)
        ));
        drop(prepared);
        assert_eq!(fs::metadata(fixture.socket()).unwrap().ino(), inode);
        assert_eq!(
            fs::read_dir(fixture.socket().parent().unwrap())
                .unwrap()
                .count(),
            1
        );
        let mut socket = fixture.connect();
        socket.write_all(&request("HEAD", "")).unwrap();
        assert!(
            fixture
                .receive(&mut socket, false)
                .starts_with(b"HTTP/1.1 200")
        );
        let prepared =
            futures::executor::block_on(fixture.listeners.prepare(&[changed], &scope())).unwrap();
        prepared.commit();
        fixture.pump(16);
        assert_eq!(
            fs::metadata(fixture.socket()).unwrap().mode() & 0o777,
            0o660
        );
        assert!(fixture.receive(&mut socket, true).is_empty());
    }

    #[test]
    fn abandoned_prepare_future_removes_temporary_socket() {
        let fixture = Fixture::new();
        fixture.reconcile(&[definition()]).unwrap();
        let inode = fs::metadata(fixture.socket()).unwrap().ino();
        let mut changed = definition();
        changed.socket_mode = 0o660;
        let definitions = [changed];
        let scope = scope();
        let mut future = fixture.listeners.prepare(&definitions, &scope);
        let waker = futures::task::noop_waker();
        assert!(
            future
                .as_mut()
                .poll(&mut Context::from_waker(&waker))
                .is_pending()
        );
        assert_eq!(
            fs::read_dir(fixture.socket().parent().unwrap())
                .unwrap()
                .count(),
            2
        );
        drop(future);
        assert_eq!(fs::metadata(fixture.socket()).unwrap().ino(), inode);
        assert_eq!(
            fs::read_dir(fixture.socket().parent().unwrap())
                .unwrap()
                .count(),
            1
        );
        assert!(!fixture.listeners.preparing.get());
    }

    #[test]
    fn prepared_rename_failure_restores_already_exchanged_paths() {
        let fixture = Fixture::new();
        fixture.reconcile(&[definition()]).unwrap();
        let inode = fs::metadata(fixture.socket()).unwrap().ino();
        let mut changed = definition();
        changed.socket_mode = 0o660;
        let mut added = definition();
        added.id = CacheId("00000000-0000-4000-8000-000000000002".into());
        added.name = "added".into();
        added.client_socket = "/run/racer/added/client/socket".into();
        added.origin_socket = "/run/racer/added/origin/socket".into();
        transition::FAIL_RENAME_AFTER.with(|count| count.set(Some(1)));
        assert!(
            futures::executor::block_on(fixture.listeners.prepare(&[changed, added], &scope()))
                .is_err()
        );
        assert_eq!(fs::metadata(fixture.socket()).unwrap().ino(), inode);
        assert_eq!(
            fs::metadata(fixture.socket()).unwrap().mode() & 0o777,
            0o600
        );
        assert_eq!(
            fs::read_dir(fixture.socket().parent().unwrap())
                .unwrap()
                .count(),
            1
        );
        assert_eq!(
            fs::read_dir(fixture.root.0.join("added/client"))
                .unwrap()
                .count(),
            0
        );
        let mut socket = fixture.connect();
        socket
            .write_all(&request("HEAD", "Connection: close\r\n"))
            .unwrap();
        assert!(
            fixture
                .receive(&mut socket, true)
                .starts_with(b"HTTP/1.1 200")
        );
    }

    #[test]
    fn prepared_uid_reuse_and_foreign_replacement_are_inode_safe() {
        let fixture = Fixture::new();
        fixture.reconcile(&[definition()]).unwrap();
        let origin = fixture.root.0.join("example/origin");
        fs::create_dir(&origin).unwrap();
        fs::write(origin.join("socket"), b"origin").unwrap();
        let mut replacement = definition();
        replacement.id = CacheId("00000000-0000-4000-8000-000000000002".into());
        let prepared = futures::executor::block_on(
            fixture.listeners.prepare(&[replacement.clone()], &scope()),
        )
        .unwrap();
        prepared.commit();
        fixture.pump(16);
        let inode = fs::metadata(fixture.socket()).unwrap().ino();
        assert!(
            fixture
                .listeners
                .listeners
                .borrow()
                .contains_key(&replacement.id)
        );
        replacement.socket_mode = 0o660;
        let prepared =
            futures::executor::block_on(fixture.listeners.prepare(&[replacement], &scope()))
                .unwrap();
        fs::remove_file(fixture.socket()).unwrap();
        fs::write(fixture.socket(), b"foreign").unwrap();
        drop(prepared);
        assert_eq!(fs::read(fixture.socket()).unwrap(), b"foreign");
        assert_eq!(fs::read(origin.join("socket")).unwrap(), b"origin");
        assert!(
            fs::read_dir(fixture.socket().parent().unwrap())
                .unwrap()
                .any(|entry| entry.unwrap().metadata().unwrap().ino() == inode)
        );
    }

    #[test]
    fn per_cache_cancellation_drains_active_read_and_keeps_other_cache() {
        struct Waiting(Rc<Cell<usize>>);
        impl ReadService for Waiting {
            fn read<'a>(
                &'a self,
                _: ClientRequest,
                scope: &'a RequestScope,
            ) -> Operation<'a, ReadResponse> {
                Box::pin(async move {
                    self.0.set(self.0.get() + 1);
                    std::future::poll_fn(|_| match scope.check() {
                        Ok(()) => Poll::Pending,
                        Err(error) => Poll::Ready(Err(error)),
                    })
                    .await
                })
            }
        }
        let mut fixture = Fixture::new();
        let calls = Rc::new(Cell::new(0));
        fixture.listeners.reads = Rc::new(Waiting(calls.clone()));
        let mut other = definition();
        other.id = CacheId("00000000-0000-4000-8000-000000000002".into());
        other.name = "other".into();
        other.client_socket = "/run/racer/other/client/socket".into();
        other.origin_socket = "/run/racer/other/origin/socket".into();
        fixture.reconcile(&[definition(), other.clone()]).unwrap();
        let mut socket = fixture.connect();
        socket.write_all(&request("HEAD", "")).unwrap();
        for _ in 0..16 {
            fixture.pump(16);
        }
        assert_eq!(calls.get(), 1);
        assert_eq!(
            fixture.listeners.active_connections_for(&definition().id),
            1
        );
        fixture.listeners.cancel_cache(&definition().id).unwrap();
        assert!(
            fixture
                .receive(&mut socket, true)
                .starts_with(b"HTTP/1.1 503")
        );
        futures::executor::block_on(fixture.listeners.drain_cache(&definition().id, &scope()))
            .unwrap();
        assert_eq!(
            fixture.listeners.active_connections_for(&definition().id),
            0
        );
        assert!(fixture.listeners.listeners.borrow().contains_key(&other.id));
        assert!(fixture.root.0.join("other/client/socket").exists());
    }

    #[test]
    fn socket_lifecycle_rejects_symlinks_and_preserves_unowned_paths() {
        let fixture = Fixture::new();
        let origin = fixture.root.0.join("example/origin");
        fs::create_dir_all(&origin).unwrap();
        fs::write(origin.join("socket"), b"adapter owned").unwrap();
        fixture.reconcile(&[definition()]).unwrap();
        let mut updated = definition();
        updated.socket_mode = 0o660;
        fixture.reconcile(&[updated]).unwrap();
        assert_eq!(
            fs::metadata(fixture.socket()).unwrap().mode() & 0o777,
            0o660
        );
        // Replacing the pathname does not give us ownership of its replacement.
        fs::remove_file(fixture.socket()).unwrap();
        fs::write(fixture.socket(), b"replacement").unwrap();
        fixture.reconcile(&[]).unwrap();
        assert_eq!(fs::read(fixture.socket()).unwrap(), b"replacement");
        assert_eq!(fs::read(origin.join("socket")).unwrap(), b"adapter owned");
        assert_eq!(fixture.reconcile(&[definition()]), Err(Error::Io));
        fs::remove_file(fixture.socket()).unwrap();
        fs::remove_dir(fixture.socket().parent().unwrap()).unwrap();
        std::os::unix::fs::symlink(&origin, fixture.socket().parent().unwrap()).unwrap();
        assert_eq!(fixture.reconcile(&[definition()]), Err(Error::Io));
        assert_eq!(fs::read(origin.join("socket")).unwrap(), b"adapter owned");
        let mut invalid = definition();
        invalid.name = "../escape".into();
        assert_eq!(fixture.reconcile(&[invalid]), Err(Error::InvalidRequest));
    }
}
