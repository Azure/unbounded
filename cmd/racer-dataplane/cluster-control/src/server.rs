//! Control API and admission webhook servers.
//!
//! The API uses exact paths and methods, canonical `after=N` cursors, and
//! payload-free JSON errors. It sets `Retry-After: 1` on 429 and 503 and
//! `Cache-Control: no-store` on responses. Successful control responses are
//! written only while this seat has authority, and their write deadlines
//! never pass the authority deadline.

use crate::{
    auth,
    authority::publication::Image,
    exec::{self, Listener, Slot, Slots},
    http::{self, Conn, Head, HttpError, TlsStream},
    limits,
    pki::{self, PkiError},
    shared::Shared,
};
use racer_control_wire::{self as wire, ErrorResponse, ProtocolFailure};
use std::{
    cell::RefCell,
    collections::HashSet,
    rc::Rc,
    sync::Arc,
    time::{Duration, Instant, SystemTime, UNIX_EPOCH},
};

/// Primary-only route that seats use to copy the committed image.
pub const REPLICA_PATH: &str = "/internal/v1/replica";

/// Header naming the client's delta base content hash.
pub const DELTA_HEADER: &str = "X-Racer-Delta-Base";

/// Plaintext written per step; authority is rechecked before each.
const WRITE_CHUNK: usize = 64 * 1024;

fn unix_now() -> u64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap_or_default()
        .as_secs()
}

/// Admission capacities and deadlines of one server.
#[derive(Clone, Copy, Debug)]
pub struct Budget {
    pub connections: usize,
    pub handshakes: usize,
    pub bootstraps: usize,
    pub writes: usize,
    /// Wait for the first byte of a request.
    pub idle: Duration,
    /// Handshake, rest of a head, a body, and the fixed part of a write.
    pub io: Duration,
    /// Write rate floor that extends the write deadline with size.
    pub write_rate: u64,
}

impl Budget {
    /// Production budget for a descriptor limit.
    pub fn for_nofile(nofile: u64) -> Self {
        Self {
            connections: limits::connection_capacity(nofile),
            handshakes: limits::MAX_HANDSHAKES,
            bootstraps: limits::MAX_BOOTSTRAPS,
            writes: limits::MAX_WRITES,
            idle: limits::IDLE_TIMEOUT,
            io: limits::IO_TIMEOUT,
            write_rate: limits::MIN_WRITE_BYTES_PER_SEC,
        }
    }

    /// Deadline for writing `len` bytes starting now.
    fn write_deadline(&self, len: usize) -> Instant {
        Instant::now()
            + self.io
            + Duration::from_millis((len as u64).saturating_mul(1000) / self.write_rate.max(1))
    }
}

/// Per-server admission state.
pub struct Server {
    shared: Rc<Shared>,
    budget: Budget,
    connections: Slots,
    handshakes: Slots,
    bootstraps: Slots,
    writes: Slots,
    polls: RefCell<HashSet<String>>,
    keyring_polls: RefCell<HashSet<String>>,
}

struct PollGuard<'a>(&'a RefCell<HashSet<String>>, String);

impl Drop for PollGuard<'_> {
    fn drop(&mut self) {
        self.0.borrow_mut().remove(&self.1);
    }
}

/// Hold the one long poll `node` may have open, admitted by
/// [`crate::step::admission::start_poll`] against `current`.
fn admit_poll<'a>(
    set: &'a RefCell<HashSet<String>>,
    node: &str,
    after: Option<u64>,
    current: Option<u64>,
) -> Result<PollGuard<'a>, ProtocolFailure> {
    let node = node.to_string();
    start_poll(&mut set.borrow_mut(), &node, after, current)?;
    Ok(PollGuard(set, node))
}

/// Parse `after=N`. Absent means no cursor. Zero is a conflict.
pub fn cursor(query: Option<&str>) -> Result<Option<u64>, ProtocolFailure> {
    let Some(q) = query else { return Ok(None) };
    let v = q
        .strip_prefix("after=")
        .ok_or(ProtocolFailure::InvalidRequest)?;
    let n: u64 = v.parse().map_err(|_| ProtocolFailure::InvalidRequest)?;
    if n.to_string() != v {
        return Err(ProtocolFailure::InvalidRequest);
    }
    if n == 0 {
        return Err(ProtocolFailure::Conflict);
    }
    Ok(Some(n))
}

use crate::step::admission::start_poll;

fn pki_failure(e: PkiError) -> ProtocolFailure {
    match e {
        PkiError::InvalidRequest => ProtocolFailure::InvalidRequest,
        PkiError::Unauthenticated => ProtocolFailure::Unauthenticated,
        PkiError::Forbidden => ProtocolFailure::Forbidden,
        PkiError::Unavailable => ProtocolFailure::Unavailable,
    }
}

/// Encoded error response.
pub fn error_response(code: ProtocolFailure) -> Vec<u8> {
    let mut r = error_reply(code);
    let mut out = std::mem::take(&mut r.head);
    out.extend_from_slice(&r.body);
    out
}

fn error_reply(code: ProtocolFailure) -> Reply<'static> {
    let body = wire::encode_error(&ErrorResponse { code }).unwrap_or_default();
    let mut headers = vec![
        ("Content-Type", "application/json"),
        ("Cache-Control", "no-store"),
    ];
    if matches!(
        code,
        ProtocolFailure::Unavailable | ProtocolFailure::Overloaded
    ) {
        headers.push(("Retry-After", "1"));
    }
    Reply {
        head: http::response_head(code.status(), &headers, body.len(), false),
        body: Arc::new(body),
        authorized: false,
        close: false,
        caller: None,
        _write: None,
        _admission: None,
        _poll: None,
    }
}

/// A response. The body is shared with the published image, never copied
/// per connection. Held permits (write, admission, per-Node poll) are
/// released only after the last byte is written or the write fails.
struct Reply<'a> {
    head: Vec<u8>,
    body: Arc<Vec<u8>>,
    /// Written only while this seat has authority.
    authorized: bool,
    /// Close after writing.
    close: bool,
    /// The caller's credential expiry. No byte is written after it.
    caller: Option<Instant>,
    _write: Option<Slot>,
    _admission: Option<Slot>,
    _poll: Option<PollGuard<'a>>,
}

/// Local deadline of a credential expiring at `expires` (Unix seconds).
fn caller_deadline(expires: u64) -> Instant {
    Instant::now() + Duration::from_secs(expires.saturating_sub(unix_now()))
}

/// Write fence: the seat authority deadline, clamped to the caller's.
fn fence(seat: Option<Instant>, caller: Option<Instant>) -> Option<Instant> {
    match (seat, caller) {
        (Some(s), Some(c)) => Some(s.min(c)),
        (s, None) => s,
        (None, _) => None,
    }
}

impl<'a> Reply<'a> {
    /// Stop writing when the caller's credential expires.
    fn until(mut self, expires: u64) -> Self {
        self.caller = Some(caller_deadline(expires));
        self
    }

    fn admitted(mut self, slot: Slot) -> Self {
        self._admission = Some(slot);
        self
    }

    fn polling(mut self, guard: PollGuard<'a>) -> Self {
        self._poll = Some(guard);
        self
    }

    fn ok(body: Arc<Vec<u8>>, write: Slot) -> Self {
        Self {
            head: http::response_head(
                200,
                &[
                    ("Content-Type", "application/json"),
                    ("Cache-Control", "no-store"),
                ],
                body.len(),
                false,
            ),
            body,
            authorized: true,
            close: false,
            caller: None,
            _write: Some(write),
            _admission: None,
            _poll: None,
        }
    }

    fn no_content() -> Self {
        Self {
            head: http::response_head(204, &[("Cache-Control", "no-store")], 0, false),
            body: Arc::default(),
            authorized: true,
            close: false,
            caller: None,
            _write: None,
            _admission: None,
            _poll: None,
        }
    }
}

/// Why a request produced no normal reply.
enum Fail {
    Code(ProtocolFailure),
    /// Drop the connection without a reply, as for a stalled body.
    Drop,
}

impl From<ProtocolFailure> for Fail {
    fn from(c: ProtocolFailure) -> Self {
        Self::Code(c)
    }
}

/// Write `head` then `body` by `deadline`. With `authorized`, every chunk is
/// written only while `authority` returns a deadline, and never past it: a
/// seat that loses authority mid-response stops and the connection closes,
/// so the client sees a truncated response, never a stale complete one.
pub(crate) async fn write_fenced(
    tls: &mut TlsStream,
    head: &[u8],
    body: &[u8],
    deadline: Instant,
    authorized: bool,
    authority: impl Fn() -> Option<Instant>,
) -> bool {
    let mut parts = std::iter::once(head).chain(body.chunks(WRITE_CHUNK));
    loop {
        let Some(part) = parts.next() else {
            return true;
        };
        let at = if authorized {
            match authority() {
                Some(a) if a > Instant::now() => deadline.min(a),
                _ => return false,
            }
        } else {
            deadline
        };
        if !matches!(exec::deadline(at, tls.write_all(part)).await, Ok(Ok(()))) {
            return false;
        }
    }
}

fn json_request(head: &Head) -> bool {
    head.one("content-type")
        .ok()
        .flatten()
        .and_then(|v| v.split(';').next())
        .is_some_and(|m| m.trim().eq_ignore_ascii_case("application/json"))
        && head.count("content-encoding") == 0
}

fn empty_body(head: &Head) -> bool {
    head.count("transfer-encoding") == 0
        && head
            .one("content-length")
            .is_ok_and(|v| v.is_none_or(|v| v == "0"))
}

fn wants_close(head: &Head) -> bool {
    head.one("connection")
        .ok()
        .flatten()
        .is_some_and(|v| v.eq_ignore_ascii_case("close"))
}

/// Read the next request head: up to `idle` for the first byte, then `io`
/// for the rest, so a slow head cannot hold a connection past `idle + io`.
async fn next_head(conn: &mut Conn, idle: Duration, io: Duration) -> Result<Head, Option<Vec<u8>>> {
    match exec::timeout(idle, conn.wait_for_data()).await {
        Ok(Ok(true)) => {}
        _ => return Err(None),
    }
    match exec::timeout(io, conn.read_head(true, limits::MAX_HEAD_BYTES)).await {
        Ok(Ok(Some(h))) => Ok(h),
        Ok(Err(HttpError::TooLarge)) => Err(Some(error_response(ProtocolFailure::TooLarge))),
        _ => Err(None),
    }
}

impl Server {
    pub fn new(shared: Rc<Shared>) -> Rc<Self> {
        Self::with_budget(shared, Budget::for_nofile(limits::raise_nofile()))
    }

    pub fn with_budget(shared: Rc<Shared>, budget: Budget) -> Rc<Self> {
        Rc::new(Self {
            shared,
            budget,
            connections: Slots::new(budget.connections),
            handshakes: Slots::new(budget.handshakes),
            bootstraps: Slots::new(budget.bootstraps),
            writes: Slots::queued(budget.writes, limits::MAX_WRITE_WAITERS),
            polls: RefCell::new(HashSet::new()),
            keyring_polls: RefCell::new(HashSet::new()),
        })
    }

    /// Accept control connections until the runtime stops.
    pub async fn run(self: Rc<Self>, listener: Listener) {
        while !self.shared.stopping.get() {
            let Ok((tcp, _)) = listener.accept().await else {
                exec::sleep(Duration::from_millis(100)).await;
                continue;
            };
            // Over capacity: close at once. Clients retry with backoff.
            let Some(slot) = self.connections.try_acquire() else {
                continue;
            };
            let Some(handshake) = self.handshakes.try_acquire() else {
                continue;
            };
            // Only seat holders serve. Others refuse at once.
            let Some(tls) = self
                .shared
                .seat
                .borrow()
                .as_ref()
                .map(|s| s.control_tls.borrow().clone())
            else {
                continue;
            };
            let this = self.clone();
            let _ = exec::spawn(async move {
                let _slot = slot;
                let stream = exec::timeout(this.budget.io, TlsStream::server(tcp, tls)).await;
                drop(handshake);
                let Ok(Ok(stream)) = stream else {
                    return;
                };
                this.connection(Conn::new(stream)).await;
            });
        }
    }

    async fn connection(&self, mut conn: Conn) {
        loop {
            let head = match next_head(&mut conn, self.budget.idle, self.budget.io).await {
                Ok(h) => h,
                Err(Some(reply)) => {
                    let _ = exec::timeout(self.budget.io, conn.tls.write_all(&reply)).await;
                    return;
                }
                Err(None) => return,
            };
            let mut reply = match self.route(&mut conn, &head).await {
                Ok(r) => r,
                Err(Fail::Drop) => return,
                Err(Fail::Code(code)) => {
                    let mut r = error_reply(code);
                    // The body may be unread: do not parse it as a request.
                    r.close = !empty_body(&head);
                    r
                }
            };
            reply.close |= wants_close(&head) || self.shared.stopping.get();
            let deadline = self
                .budget
                .write_deadline(reply.head.len() + reply.body.len());
            let shared = &self.shared;
            let caller = reply.caller;
            let authority = || {
                let seat = if shared.has_authority() {
                    shared.authority().deadline()
                } else {
                    None
                };
                fence(seat, caller)
            };
            if !write_fenced(
                &mut conn.tls,
                &reply.head,
                &reply.body,
                deadline,
                reply.authorized,
                authority,
            )
            .await
            {
                return;
            }
            let close = reply.close;
            // Permits are released only now, after the last byte.
            drop(reply);
            if close || self.shared.stopping.get() {
                let _ = exec::timeout(self.budget.io, conn.tls.close()).await;
                return;
            }
        }
    }

    async fn route(&self, conn: &mut Conn, head: &Head) -> Result<Reply<'_>, Fail> {
        let (path, query) = http::split_target(&head.target);
        if head.target.contains('%') || head.target.contains('#') {
            return Err(ProtocolFailure::InvalidRequest.into());
        }
        match (head.method.as_str(), path) {
            ("POST", wire::BOOTSTRAP_PATH) => {
                if query.is_some() || !json_request(head) {
                    return Err(ProtocolFailure::InvalidRequest.into());
                }
                // Admit before reading: at most `bootstraps` bodies are
                // buffered, however many connections are open.
                let admission = self
                    .bootstraps
                    .try_acquire()
                    .ok_or(ProtocolFailure::Overloaded)?;
                let body = match exec::timeout(
                    self.budget.io,
                    conn.read_request_body(head, wire::MAX_ENROLLMENT_BYTES),
                )
                .await
                {
                    Ok(Ok(b)) => b,
                    Ok(Err(HttpError::TooLarge)) => return Err(ProtocolFailure::TooLarge.into()),
                    Ok(Err(HttpError::Invalid)) => {
                        return Err(ProtocolFailure::InvalidRequest.into());
                    }
                    Ok(Err(HttpError::Io)) | Err(_) => return Err(Fail::Drop),
                };
                let mut r = self
                    .bootstrap(head, &body)
                    .await
                    .map(|r| r.admitted(admission));
                // The body was consumed: the connection stays usable.
                if let Err(Fail::Code(code)) = r {
                    r = Ok(error_reply(code));
                }
                r
            }
            ("GET", wire::SNAPSHOT_PATH) => {
                if !empty_body(head) {
                    return Err(ProtocolFailure::InvalidRequest.into());
                }
                let after = cursor(query)?;
                let chain = conn.tls.peer_certificates();
                Ok(self.snapshot(head, after, &chain).await?)
            }
            ("GET", wire::KEYRING_PATH) => {
                if !empty_body(head) || head.count("content-encoding") != 0 {
                    return Err(ProtocolFailure::InvalidRequest.into());
                }
                let after = cursor(query)?;
                let chain = conn.tls.peer_certificates();
                Ok(self.keyring(head, after, &chain).await?)
            }
            ("GET", REPLICA_PATH) => {
                if !empty_body(head) {
                    return Err(ProtocolFailure::InvalidRequest.into());
                }
                let after = cursor(query)?;
                Ok(self.replica(head, after).await?)
            }
            _ => Err(ProtocolFailure::InvalidRequest.into()),
        }
    }

    fn write_slot(&self) -> Result<Slot, ProtocolFailure> {
        self.writes.try_acquire().ok_or(ProtocolFailure::Overloaded)
    }

    /// Write slot for a long poll reply. Waits in FIFO order until
    /// `poll_deadline` plus [`limits::WRITE_QUEUE_GRACE`], clamped to seat
    /// authority and `expires` (the caller's credential), so a mass wake
    /// queues instead of answering 429. A full queue or a passed deadline is
    /// overload and still answers 429. Authority is rechecked after waiting.
    async fn queued_write_slot(
        &self,
        poll_deadline: Instant,
        expires: u64,
    ) -> Result<Slot, ProtocolFailure> {
        let mut at = (poll_deadline + limits::WRITE_QUEUE_GRACE).min(caller_deadline(expires));
        if let Some(a) = self.shared.authority().deadline() {
            at = at.min(a);
        }
        let slot = self
            .writes
            .acquire(at)
            .await
            .ok_or(ProtocolFailure::Overloaded)?;
        if !self.shared.ready() {
            return Err(ProtocolFailure::Unavailable);
        }
        Ok(slot)
    }

    /// Enrollment. The caller holds a bootstrap admission slot.
    async fn bootstrap(&self, head: &Head, body: &[u8]) -> Result<Reply<'static>, Fail> {
        if !self.shared.ready() {
            return Err(ProtocolFailure::Unavailable.into());
        }
        let request = wire::decode_enrollment_request(body).map_err(|e| match e {
            wire::Error::Overloaded => ProtocolFailure::TooLarge,
            _ => ProtocolFailure::InvalidRequest,
        })?;
        let cluster = self.shared.cluster.borrow().clone();
        if request.cluster.0 != cluster {
            return Err(ProtocolFailure::Forbidden.into());
        }
        let authorization = head
            .one("authorization")
            .map_err(|_| ProtocolFailure::Unauthenticated)?;
        let caller = exec::timeout(
            self.budget.io,
            auth::authorize(&self.shared, authorization, unix_now()),
        )
        .await
        .map_err(|_| ProtocolFailure::Unavailable)??;
        // Recheck after API calls: authority may have lapsed meanwhile.
        if !self.shared.ready() {
            return Err(ProtocolFailure::Unavailable.into());
        }
        let seat = self
            .shared
            .seat
            .borrow()
            .clone()
            .ok_or(ProtocolFailure::Unavailable)?;
        let write = self.write_slot()?;
        let chain = seat
            .issuer
            .borrow()
            .issue(
                &cluster,
                &caller.node.uid,
                &request.csr_der,
                SystemTime::now(),
            )
            .map_err(pki_failure)?;
        let response = wire::EnrollmentResponse {
            block_devices: block_devices(&caller.node.annotations),
            schema_version: wire::SCHEMA_VERSION,
            cluster: request.cluster,
            node: wire::NodeId(caller.node.uid.clone()),
            enrollment: request.enrollment,
            certificate_chain: chain,
        };
        let body = wire::encode_enrollment_response(&response)
            .map_err(|_| ProtocolFailure::Unavailable)?;
        Ok(Reply::ok(Arc::new(body), write).until(caller.expires))
    }

    fn certificate(&self, chain: &[Vec<u8>]) -> Result<pki::NodeIdentity, ProtocolFailure> {
        if chain.is_empty() {
            return Err(ProtocolFailure::Unauthenticated);
        }
        let cluster = self.shared.cluster.borrow().clone();
        pki::authenticate_client(chain, &self.shared.roots(), &cluster, SystemTime::now())
            .map_err(pki_failure)
    }

    /// Wait for `ready` or until `deadline`, waking on changes.
    async fn wait_until<T>(
        &self,
        deadline: Instant,
        mut ready: impl FnMut() -> Option<T>,
    ) -> Option<T> {
        loop {
            if let Some(v) = ready() {
                return Some(v);
            }
            if Instant::now() >= deadline || self.shared.stopping.get() {
                return None;
            }
            let generation = self.shared.changed.generation();
            if let Some(v) = ready() {
                return Some(v);
            }
            let _ = exec::deadline(deadline, self.shared.changed.changed(generation)).await;
        }
    }

    fn poll_deadline(&self, expires: u64) -> Result<Instant, ProtocolFailure> {
        let now = unix_now();
        if expires <= now {
            return Err(ProtocolFailure::Unauthenticated);
        }
        let authority = self
            .shared
            .authority()
            .deadline()
            .ok_or(ProtocolFailure::Unavailable)?;
        Ok((Instant::now() + wire::POLL_WAIT)
            .min(Instant::now() + Duration::from_secs(expires - now))
            .min(authority))
    }

    async fn snapshot(
        &self,
        head: &Head,
        after: Option<u64>,
        chain: &[Vec<u8>],
    ) -> Result<Reply<'_>, ProtocolFailure> {
        let id = self.certificate(chain)?;
        let current = self.shared.image().map(|i| i.record.sequence);
        let poll = admit_poll(&self.polls, &id.node, after, current)?;
        let deadline = self.poll_deadline(id.expires)?;
        let newer = |i: &Image| crate::step::admission::newer(after, i.record.sequence);
        let found = self
            .wait_until(deadline, || self.shared.image().filter(newer))
            .await;
        // Recheck certificate trust and authority after waiting.
        let id2 = self.certificate(chain)?;
        if id2.node != id.node {
            return Err(ProtocolFailure::Forbidden);
        }
        if !self.shared.ready() {
            return Err(ProtocolFailure::Unavailable);
        }
        let reply = match found {
            None => Reply::no_content(),
            Some(image) => {
                let write = self
                    .queued_write_slot(deadline, id2.expires.min(id.expires))
                    .await?;
                // The slot wait may be long: recheck the certificate, then
                // readiness, before data leaves.
                if self.certificate(chain)?.node != id.node {
                    return Err(ProtocolFailure::Forbidden);
                }
                if !self.shared.ready() {
                    return Err(ProtocolFailure::Unavailable);
                }
                // A newer image may have landed while queued.
                let image = self.shared.image().filter(newer).unwrap_or(image);
                let base = head.one(DELTA_HEADER).ok().flatten();
                Reply::ok(image.for_base(after, base), write)
            }
        };
        Ok(reply.until(id2.expires.min(id.expires)).polling(poll))
    }

    /// Full bearer authorization under the bootstrap budget.
    async fn authorize_bearer(&self, head: &Head) -> Result<auth::Caller, ProtocolFailure> {
        let _slot = self
            .bootstraps
            .try_acquire()
            .ok_or(ProtocolFailure::Overloaded)?;
        let authorization = head
            .one("authorization")
            .map_err(|_| ProtocolFailure::Unauthenticated)?;
        exec::timeout(
            self.budget.io,
            auth::authorize(&self.shared, authorization, unix_now()),
        )
        .await
        .map_err(|_| ProtocolFailure::Unavailable)?
    }

    async fn keyring(
        &self,
        head: &Head,
        after: Option<u64>,
        chain: &[Vec<u8>],
    ) -> Result<Reply<'_>, ProtocolFailure> {
        let bearer = head.count("authorization") != 0;
        if bearer && !chain.is_empty() {
            return Err(ProtocolFailure::Unauthenticated);
        }
        let authorized_at = Instant::now();
        let (node, expires) = if bearer {
            let c = self.authorize_bearer(head).await?;
            (c.node.uid, c.expires)
        } else {
            let id = self.certificate(chain)?;
            (id.node, id.expires)
        };
        let current = self.shared.bundle().map(|(g, _)| g);
        let poll = admit_poll(&self.keyring_polls, &node, after, current)?;
        let deadline = self.poll_deadline(expires)?;
        let newer = || {
            self.shared
                .bundle()
                .filter(|(g, _)| after.is_none_or(|a| *g > a))
        };
        // Data already there: the authorization above is current, so the
        // bearer recheck below may be skipped if the write slot comes
        // quickly. An unenrolled client's first keyring poll (no cursor)
        // takes this path.
        let immediate = newer().is_some();
        let found = self.wait_until(deadline, newer).await;
        let reply = match found {
            None => {
                self.recheck_keyring_caller(head, chain, bearer, &node, expires, false)
                    .await?;
                Reply::no_content()
            }
            Some((_, bytes)) => {
                // The slot wait may outlast the authorization cache, so the
                // caller is rechecked after it, then readiness after that.
                let write = self.queued_write_slot(deadline, expires).await?;
                let full = bearer_recheck(true, immediate, authorized_at.elapsed());
                self.recheck_keyring_caller(head, chain, bearer, &node, expires, full)
                    .await?;
                Reply::ok(bytes, write)
            }
        };
        Ok(reply.until(expires).polling(poll))
    }

    /// Recheck a keyring caller before replying, then readiness. `full`
    /// re-runs bearer authorization (normally served by the cache); without
    /// it a bearer caller is only checked for token expiry. An empty 204
    /// reveals nothing, so it needs no API calls, which keeps idle bearer
    /// polls from amplifying into reviews.
    async fn recheck_keyring_caller(
        &self,
        head: &Head,
        chain: &[Vec<u8>],
        bearer: bool,
        node: &str,
        expires: u64,
        full: bool,
    ) -> Result<(), ProtocolFailure> {
        if !bearer {
            let again = self.certificate(chain)?;
            if again.node != node {
                return Err(ProtocolFailure::Forbidden);
            }
        } else if expires <= unix_now() {
            return Err(ProtocolFailure::Unauthenticated);
        } else if full {
            let again = self.authorize_bearer(head).await?;
            if again.node.uid != node {
                return Err(ProtocolFailure::Forbidden);
            }
        }
        if !self.shared.ready() {
            return Err(ProtocolFailure::Unavailable);
        }
        Ok(())
    }

    async fn replica(
        &self,
        head: &Head,
        after: Option<u64>,
    ) -> Result<Reply<'static>, ProtocolFailure> {
        if !self.shared.is_primary.get() {
            return Err(ProtocolFailure::Unavailable);
        }
        let authorization = head
            .one("authorization")
            .map_err(|_| ProtocolFailure::Unauthenticated)?;
        let expires = exec::timeout(
            self.budget.io,
            auth::authorize_seat(&self.shared, authorization, unix_now()),
        )
        .await
        .map_err(|_| ProtocolFailure::Unavailable)??;
        let deadline = self.poll_deadline(expires)?;
        let found = self
            .wait_until(deadline, || {
                self.shared
                    .image()
                    .filter(|i| after.is_none_or(|a| i.record.sequence > a))
            })
            .await;
        if !self.shared.ready() {
            return Err(ProtocolFailure::Unavailable);
        }
        require_primary(self.shared.is_primary.get())?;
        let reply = match found {
            None => Reply::no_content(),
            Some(i) => {
                let slot = self.queued_write_slot(deadline, expires).await?;
                // Recheck the seat caller after the slot wait (the review
                // is normally cached). The primary term can also end while
                // this reply waits, and a follower seat stays ready, so
                // recheck the role, then readiness.
                exec::timeout(
                    self.budget.io,
                    auth::authorize_seat(&self.shared, authorization, unix_now()),
                )
                .await
                .map_err(|_| ProtocolFailure::Unavailable)??;
                require_primary(self.shared.is_primary.get())?;
                if !self.shared.ready() {
                    return Err(ProtocolFailure::Unavailable);
                }
                Reply::ok(i.encoded.clone(), slot)
            }
        };
        Ok(reply.until(expires))
    }
}

/// Replica images are served only while this process holds the primary
/// term.
fn require_primary(is_primary: bool) -> Result<(), ProtocolFailure> {
    if is_primary {
        Ok(())
    } else {
        Err(ProtocolFailure::Unavailable)
    }
}

/// Valid block device pattern from Node annotations, as the old controller.
pub fn block_devices(annotations: &std::collections::BTreeMap<String, String>) -> Option<String> {
    let v = annotations.get(crate::annotations::BLOCK_DEVICES)?;
    (!v.is_empty() && v.len() <= 1024 && regex::Regex::new(v).is_ok()).then(|| v.clone())
}

/// Concurrent admission bodies held in memory, each up to
/// [`racer_admission::MAX_REQUEST_BYTES`].
const MAX_ADMISSION_BODIES: usize = 8;

/// One webhook listener with its own budget, for tests.
#[cfg(test)]
pub(crate) async fn run_webhook_with(
    shared: Rc<Shared>,
    listener: Listener,
    bodies: usize,
    idle: Duration,
    io: Duration,
) {
    let budget = WebhookBudget::new(limits::MAX_WEBHOOK_CONNECTIONS, bodies);
    serve_webhook(shared, listener, budget, idle, io).await
}

/// Webhook connection and admission body slots. One budget is shared by
/// every webhook listener, so serving more than one Pod IP family never
/// raises the bounds that [`limits`] reserves descriptors and memory for.
#[derive(Clone)]
pub(crate) struct WebhookBudget {
    connections: Slots,
    bodies: Slots,
}

impl WebhookBudget {
    pub(crate) fn new(connections: usize, bodies: usize) -> Self {
        Self {
            connections: Slots::new(connections),
            bodies: Slots::new(bodies),
        }
    }
}

/// Serve the webhook on each listener under one shared budget.
pub fn spawn_webhooks(
    shared: &Rc<Shared>,
    listeners: impl IntoIterator<Item = Listener>,
) -> Result<(), crate::Error> {
    let budget = WebhookBudget::new(limits::MAX_WEBHOOK_CONNECTIONS, MAX_ADMISSION_BODIES);
    for listener in listeners {
        exec::spawn(serve_webhook(
            shared.clone(),
            listener,
            budget.clone(),
            limits::IDLE_TIMEOUT,
            limits::IO_TIMEOUT,
        ))?;
    }
    Ok(())
}

pub(crate) async fn serve_webhook(
    shared: Rc<Shared>,
    listener: Listener,
    budget: WebhookBudget,
    idle: Duration,
    io: Duration,
) {
    let WebhookBudget {
        connections: slots,
        bodies,
    } = budget;
    while !shared.stopping.get() {
        let Ok((tcp, _)) = listener.accept().await else {
            exec::sleep(Duration::from_millis(100)).await;
            continue;
        };
        let Some(slot) = slots.try_acquire() else {
            continue;
        };
        let Some(tls) = shared
            .seat
            .borrow()
            .as_ref()
            .map(|s| s.webhook_tls.borrow().clone())
        else {
            continue;
        };
        let shared = shared.clone();
        let bodies = bodies.clone();
        let _ = exec::spawn(async move {
            let _slot = slot;
            let Ok(Ok(stream)) = exec::timeout(io, TlsStream::server(tcp, tls)).await else {
                return;
            };
            webhook_connection(&shared, &bodies, Conn::new(stream), idle, io).await;
        });
    }
}

fn status(code: u16, close: bool) -> Vec<u8> {
    let headers: &[(&str, &str)] = if code == 503 || code == 429 {
        &[("Retry-After", "1")]
    } else {
        &[]
    };
    http::response_head(code, headers, 0, close)
}

/// Route checks done before any body byte is read.
fn webhook_route(head: &Head) -> Result<(), u16> {
    let (path, query) = http::split_target(&head.target);
    // Kubernetes appends its webhook deadline as whole seconds. Keep our own
    // I/O limits, and accept no other query, fragment, or encoded path.
    let query_valid = query.is_none_or(|q| {
        let Some(seconds) = q.strip_prefix("timeout=").and_then(|v| v.strip_suffix('s')) else {
            return false;
        };
        !seconds.starts_with('0')
            && seconds.bytes().all(|b| b.is_ascii_digit())
            && seconds.parse::<u8>().is_ok_and(|v| (1..=30).contains(&v))
    });
    if path != racer_admission::PATH || !query_valid {
        return Err(404);
    }
    if head.method != "POST" {
        return Err(405);
    }
    if !json_request(head) {
        return Err(415);
    }
    Ok(())
}

async fn webhook_connection(
    shared: &Shared,
    bodies: &Slots,
    mut conn: Conn,
    idle: Duration,
    io: Duration,
) {
    loop {
        let head = match next_head(&mut conn, idle, io).await {
            Ok(h) => h,
            Err(Some(_)) => {
                let _ = exec::timeout(io, conn.tls.write_all(&status(413, true))).await;
                return;
            }
            Err(None) => return,
        };
        // Held from before the body read until the reply is written.
        let mut permit = None;
        let reply = match webhook_route(&head) {
            // The body is unread: answer and close.
            Err(code) => Err(status(code, true)),
            Ok(()) => match bodies.try_acquire() {
                None => Err(status(429, true)),
                Some(slot) => {
                    permit = Some(slot);
                    match exec::timeout(
                        io,
                        conn.read_request_body(&head, racer_admission::MAX_REQUEST_BYTES),
                    )
                    .await
                    {
                        Ok(Ok(body)) => Ok(webhook_reply(shared, &body)),
                        Ok(Err(HttpError::TooLarge)) => Err(status(413, true)),
                        Ok(Err(HttpError::Invalid)) => Err(status(400, true)),
                        _ => return,
                    }
                }
            },
        };
        let (bytes, close) = match reply {
            Ok(b) => (b, wants_close(&head)),
            Err(b) => (b, true),
        };
        // Admissions are authority-fenced like control responses. Refusals
        // carry no decision and are written regardless.
        let authority = || {
            if shared.has_authority() {
                shared.authority().deadline()
            } else {
                None
            }
        };
        let authorized = bytes.starts_with(b"HTTP/1.1 200");
        let deadline = Instant::now() + io;
        if !write_fenced(&mut conn.tls, &bytes, &[], deadline, authorized, authority).await {
            return;
        }
        drop(permit);
        if close || shared.stopping.get() {
            let _ = exec::timeout(io, conn.tls.close()).await;
            return;
        }
    }
}

/// Admission response bytes for one review body.
pub fn webhook_reply(shared: &Shared, body: &[u8]) -> Vec<u8> {
    let Some(image) = shared.config.object_image.as_deref() else {
        return status(503, false);
    };
    if !shared.has_authority() {
        return status(503, false);
    }
    match racer_admission::admit(body, image) {
        Ok(out) => {
            let mut r = http::response_head(
                200,
                &[("Content-Type", "application/json")],
                out.len(),
                false,
            );
            r.extend(out);
            r
        }
        Err(_) => http::response_head(400, &[], 0, false),
    }
}

/// Check a server certificate against a pin before any credential is sent.
///
/// The presented end entity must equal the pin byte for byte, arrive without
/// intermediates, be within validity, carry a valid self-signature, not be a
/// CA, allow serverAuth, and name `name` in its SANs. The pin alone proves
/// key possession through the handshake; the rest refuses a mis-issued or
/// repurposed certificate even if it was advertised.
pub fn check_pinned(
    pin: &[u8],
    end_entity: &[u8],
    intermediates: usize,
    name: &rustls::pki_types::ServerName<'_>,
    now: u64,
) -> Result<(), rustls::Error> {
    use rustls::{CertificateError, Error};
    use x509_parser::extensions::GeneralName;
    let bad = |e: CertificateError| Error::InvalidCertificate(e);
    if end_entity != pin || intermediates != 0 {
        return Err(bad(CertificateError::UnknownIssuer));
    }
    let (rest, c) =
        x509_parser::parse_x509_certificate(pin).map_err(|_| bad(CertificateError::BadEncoding))?;
    if !rest.is_empty() {
        return Err(bad(CertificateError::BadEncoding));
    }
    let t = i64::try_from(now).unwrap_or(i64::MAX);
    if t < c.validity().not_before.timestamp() {
        return Err(bad(CertificateError::NotValidYet));
    }
    if t >= c.validity().not_after.timestamp() {
        return Err(bad(CertificateError::Expired));
    }
    if c.issuer() != c.subject() || c.verify_signature(None).is_err() {
        return Err(bad(CertificateError::BadSignature));
    }
    match c.basic_constraints() {
        Ok(None) => {}
        Ok(Some(b)) if !b.value.ca => {}
        _ => return Err(bad(CertificateError::InvalidPurpose)),
    }
    match c.extended_key_usage() {
        Ok(Some(e)) if e.value.server_auth => {}
        _ => return Err(bad(CertificateError::InvalidPurpose)),
    }
    let names = match c.subject_alternative_name() {
        Ok(Some(san)) => san.value.general_names.clone(),
        _ => return Err(bad(CertificateError::NotValidForName)),
    };
    let matches = names.iter().any(|n| match (n, name) {
        (GeneralName::IPAddress(b), rustls::pki_types::ServerName::IpAddress(ip)) => {
            match std::net::IpAddr::from(*ip) {
                std::net::IpAddr::V4(v) => *b == v.octets().as_slice(),
                std::net::IpAddr::V6(v) => *b == v.octets().as_slice(),
            }
        }
        (GeneralName::DNSName(d), rustls::pki_types::ServerName::DnsName(n)) => {
            d.eq_ignore_ascii_case(n.as_ref())
        }
        _ => false,
    });
    if !matches {
        return Err(bad(CertificateError::NotValidForName));
    }
    Ok(())
}

/// TLS client config pinning one exact self-signed certificate. See
/// [`check_pinned`].
pub fn pinned_client(certificate: &[u8]) -> Result<Arc<rustls::ClientConfig>, crate::Error> {
    #[derive(Debug)]
    struct Pin(Vec<u8>, rustls::crypto::WebPkiSupportedAlgorithms);
    impl rustls::client::danger::ServerCertVerifier for Pin {
        fn verify_server_cert(
            &self,
            end_entity: &rustls::pki_types::CertificateDer<'_>,
            intermediates: &[rustls::pki_types::CertificateDer<'_>],
            name: &rustls::pki_types::ServerName<'_>,
            _: &[u8],
            now: rustls::pki_types::UnixTime,
        ) -> Result<rustls::client::danger::ServerCertVerified, rustls::Error> {
            check_pinned(
                &self.0,
                end_entity.as_ref(),
                intermediates.len(),
                name,
                now.as_secs(),
            )?;
            Ok(rustls::client::danger::ServerCertVerified::assertion())
        }
        fn verify_tls12_signature(
            &self,
            m: &[u8],
            c: &rustls::pki_types::CertificateDer<'_>,
            d: &rustls::DigitallySignedStruct,
        ) -> Result<rustls::client::danger::HandshakeSignatureValid, rustls::Error> {
            rustls::crypto::verify_tls12_signature(m, c, d, &self.1)
        }
        fn verify_tls13_signature(
            &self,
            m: &[u8],
            c: &rustls::pki_types::CertificateDer<'_>,
            d: &rustls::DigitallySignedStruct,
        ) -> Result<rustls::client::danger::HandshakeSignatureValid, rustls::Error> {
            rustls::crypto::verify_tls13_signature(m, c, d, &self.1)
        }
        fn supported_verify_schemes(&self) -> Vec<rustls::SignatureScheme> {
            self.1.supported_schemes()
        }
    }
    let provider = rustls::crypto::ring::default_provider();
    let algorithms = provider.signature_verification_algorithms;
    let config = rustls::ClientConfig::builder_with_provider(Arc::new(provider))
        .with_protocol_versions(&[&rustls::version::TLS13])
        .map_err(|_| crate::Error::InvalidConfiguration)?
        .dangerous()
        .with_custom_certificate_verifier(Arc::new(Pin(certificate.to_vec(), algorithms)))
        .with_no_client_auth();
    Ok(Arc::new(config))
}

/// Whether a bearer keyring poll authorizes again before replying. Only
/// data needs it: a 204 reveals nothing. Data found before waiting may
/// leave under the authorization just made, but only while that
/// authorization is younger than
/// [`crate::step::authz::CALLER_TTL`]; a long write slot
/// wait otherwise outlasts it.
fn bearer_recheck(found: bool, immediate: bool, age: Duration) -> bool {
    found && (!immediate || age >= crate::step::authz::CALLER_TTL)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::{
        exec::{Executor, Tcp},
        kube::fake,
        pki::ServingIdentity,
    };
    use rustls::pki_types::ServerName;

    const POD_IP: &str = "127.0.0.2";

    fn ip_name(ip: &str) -> ServerName<'static> {
        ServerName::IpAddress(ip.parse::<std::net::IpAddr>().unwrap().into())
    }

    fn now() -> u64 {
        unix_now()
    }

    #[test]
    fn pinned_accepts_only_a_valid_self_signed_server_leaf() {
        let t = SystemTime::now();
        let ip: std::net::IpAddr = POD_IP.parse().unwrap();
        let names = vec!["racer-webhook.racer-system.svc".to_owned()];
        let good = ServingIdentity::pinned(&[ip], &names, t, Duration::from_secs(3600)).unwrap();
        let pin = &good.leaf_der;
        check_pinned(pin, pin, 0, &ip_name(POD_IP), now()).unwrap();
        check_pinned(
            pin,
            pin,
            0,
            &ServerName::try_from("RACER-webhook.racer-system.svc").unwrap(),
            now(),
        )
        .unwrap();
        // Wrong name, intermediates, other bytes, time.
        assert!(check_pinned(pin, pin, 0, &ip_name("127.0.0.9"), now()).is_err());
        assert!(
            check_pinned(
                pin,
                pin,
                0,
                &ServerName::try_from("x.example").unwrap(),
                now()
            )
            .is_err()
        );
        assert!(check_pinned(pin, pin, 1, &ip_name(POD_IP), now()).is_err());
        let other = ServingIdentity::pinned(&[ip], &names, t, Duration::from_secs(3600)).unwrap();
        assert!(check_pinned(pin, &other.leaf_der, 0, &ip_name(POD_IP), now()).is_err());
        assert!(check_pinned(pin, pin, 0, &ip_name(POD_IP), now() + 7200).is_err());
        assert!(check_pinned(pin, pin, 0, &ip_name(POD_IP), now() - 3600).is_err());
        // A pinned CA, or a leaf signed by someone else, is refused.
        let issued =
            ServingIdentity::generate(&[ip], &names, t, Duration::from_secs(3600)).unwrap();
        let (ca, leaf) = (&issued.ca_der, &issued.leaf_der);
        assert!(check_pinned(ca, ca, 0, &ip_name(POD_IP), now()).is_err());
        assert!(check_pinned(leaf, leaf, 0, &ip_name(POD_IP), now()).is_err());
        // No serverAuth EKU.
        let key = rcgen::KeyPair::generate_for(&rcgen::PKCS_ECDSA_P256_SHA256).unwrap();
        let mut p = rcgen::CertificateParams::new(Vec::<String>::new()).unwrap();
        p.subject_alt_names.push(rcgen::SanType::IpAddress(ip));
        p.extended_key_usages = vec![rcgen::ExtendedKeyUsagePurpose::ClientAuth];
        let client_only = p.self_signed(&key).unwrap().der().to_vec();
        assert!(check_pinned(&client_only, &client_only, 0, &ip_name(POD_IP), now()).is_err());
        // Garbage pin.
        assert!(check_pinned(b"x", b"x", 0, &ip_name(POD_IP), now()).is_err());
    }

    #[test]
    fn lagging_seat_cursor_is_unavailable() {
        let e = crate::step::admission::check_ahead(Some(6), 5).unwrap_err();
        assert_eq!(e, ProtocolFailure::Unavailable);
        let r = String::from_utf8(error_response(e)).unwrap();
        assert!(r.starts_with("HTTP/1.1 503") && r.contains("Retry-After: 1"));
    }

    fn budget() -> Budget {
        Budget {
            connections: 3,
            handshakes: 2,
            bootstraps: 1,
            writes: 1,
            idle: Duration::from_millis(400),
            io: Duration::from_millis(400),
            write_rate: limits::MIN_WRITE_BYTES_PER_SEC,
        }
    }

    /// A shared state holding a seat, but not ready to serve.
    fn seated(kube: Rc<crate::kube::Kube>) -> (Rc<Shared>, Vec<u8>) {
        let shared = crate::shared::tests::shared(kube);
        let seat = crate::seat::new_seat(&shared, 0).unwrap();
        let pin = seat.control.borrow().leaf_der.clone();
        *shared.seat.borrow_mut() = Some(Rc::new(seat));
        (shared, pin)
    }

    async fn connect(address: std::net::SocketAddr, pin: &[u8]) -> std::io::Result<Conn> {
        let tcp = Tcp::connect(address).await?;
        let tls = pinned_client(pin).unwrap();
        Ok(Conn::new(
            TlsStream::client(tcp, tls, ip_name(POD_IP)).await?,
        ))
    }

    /// The server ends the connection without a response, promptly.
    async fn closed_without_reply(conn: &mut Conn) -> bool {
        matches!(
            exec::timeout(Duration::from_secs(3), conn.read_head(false, 4096)).await,
            Ok(Ok(None) | Err(_))
        )
    }

    async fn response(conn: &mut Conn) -> (u16, Vec<(String, String)>) {
        let head = exec::timeout(Duration::from_secs(3), conn.read_head(false, 4096))
            .await
            .unwrap()
            .unwrap()
            .unwrap();
        let _ = conn.read_response_body(&head, 1 << 20).await;
        (head.status, head.headers)
    }

    #[test]
    fn slow_and_stalled_requests_are_cut_and_capacity_is_bounded() {
        fake::run(|_, kube| async move {
            let (shared, pin) = seated(kube);
            let listener = Listener::bind(format!("{POD_IP}:0").parse().unwrap()).unwrap();
            let address = listener.local_addr().unwrap();
            exec::spawn(Server::with_budget(shared.clone(), budget()).run(listener)).unwrap();

            // Idle: no request byte at all.
            let started = Instant::now();
            let mut idle = connect(address, &pin).await.unwrap();
            assert!(closed_without_reply(&mut idle).await);
            assert!(started.elapsed() < Duration::from_secs(2));

            // Slowloris head: the first byte arrives, the rest never does.
            let mut slow = connect(address, &pin).await.unwrap();
            slow.tls.write_all(b"GET /v1/snap").await.unwrap();
            assert!(closed_without_reply(&mut slow).await);

            // Stalled bootstrap body.
            let mut body = connect(address, &pin).await.unwrap();
            body.tls
                .write_all(
                    format!(
                        "POST {} HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n{{",
                        wire::BOOTSTRAP_PATH
                    )
                    .as_bytes(),
                )
                .await
                .unwrap();
            assert!(closed_without_reply(&mut body).await);

            // Not ready: 503 with Retry-After, and the connection is reusable.
            let mut ok = connect(address, &pin).await.unwrap();
            for _ in 0..2 {
                ok.tls
                    .write_all(&http::request("GET", "x", wire::KEYRING_PATH, &[], &[]))
                    .await
                    .unwrap();
                let (status, headers) = response(&mut ok).await;
                assert_eq!(status, 401, "{headers:?}");
            }
            // Body sent with an error: answered, then closed.
            ok.tls
                .write_all(&http::request("POST", "x", "/nope", &[], b"{}"))
                .await
                .unwrap();
            assert_eq!(response(&mut ok).await.0, 400);
            assert!(closed_without_reply(&mut ok).await);

            // Connection cap of 3: the fourth is refused during handshake.
            let mut held = Vec::new();
            for _ in 0..3 {
                held.push(connect(address, &pin).await.unwrap());
            }
            assert!(
                exec::timeout(Duration::from_secs(3), connect(address, &pin))
                    .await
                    .unwrap()
                    .is_err()
            );
            drop(held);
            exec::sleep(Duration::from_millis(50)).await;
            // Capacity returns once connections end.
            let mut again = connect(address, &pin).await.unwrap();
            again
                .tls
                .write_all(&http::request("GET", "x", "/v1/x", &[], &[]))
                .await
                .unwrap();
            assert_eq!(response(&mut again).await.0, 400);
            shared.stopping.set(true);
        });
    }

    #[test]
    fn stalled_handshakes_are_bounded_separately() {
        fake::run(|_, kube| async move {
            let (shared, pin) = seated(kube);
            let listener = Listener::bind(format!("{POD_IP}:0").parse().unwrap()).unwrap();
            let address = listener.local_addr().unwrap();
            exec::spawn(Server::with_budget(shared.clone(), budget()).run(listener)).unwrap();
            // Two raw TCP connections that never send a ClientHello hold
            // both handshake slots.
            let a = Tcp::connect(address).await.unwrap();
            let b = Tcp::connect(address).await.unwrap();
            exec::sleep(Duration::from_millis(50)).await;
            assert!(
                exec::timeout(Duration::from_secs(3), connect(address, &pin))
                    .await
                    .unwrap()
                    .is_err()
            );
            // The handshake deadline frees them.
            exec::sleep(Duration::from_millis(500)).await;
            assert!(connect(address, &pin).await.is_ok());
            drop((a, b));
            shared.stopping.set(true);
        });
    }

    /// A TLS server writing `body` to a client that reads `read` bytes and
    /// then stops reading. Returns the server's write result and how long
    /// the write took.
    fn fenced(
        body: usize,
        read: usize,
        deadline: Duration,
        revoke_after: Option<Duration>,
        caller_after: Option<Duration>,
    ) -> (bool, Duration, usize) {
        let (server, client) = crate::http::tests::tls_pair();
        let mut ex = Executor::new(8).unwrap();
        ex.block_on(async move {
            let listener = Listener::bind("127.0.0.1:0".parse().unwrap()).unwrap();
            let address = listener.local_addr().unwrap();
            let serve = async {
                let (tcp, _) = listener.accept().await.unwrap();
                let mut tls = TlsStream::server(tcp, server).await.unwrap();
                let body = vec![7u8; body];
                let started = Instant::now();
                let revoke = revoke_after.map(|d| started + d);
                let caller = caller_after.map(|d| started + d);
                // The same composition the server uses: seat authority
                // (renewed, so always 60s ahead) clamped to the caller.
                let ok = write_fenced(&mut tls, b"HEAD", &body, started + deadline, true, || {
                    let seat = match revoke {
                        Some(r) if Instant::now() >= r => None,
                        _ => Some(Instant::now() + Duration::from_secs(60)),
                    };
                    fence(seat, caller)
                })
                .await;
                (ok, started.elapsed())
            };
            let call = async {
                let tcp = Tcp::connect(address).await.unwrap();
                let mut tls =
                    TlsStream::client(tcp, client, ServerName::try_from("localhost").unwrap())
                        .await
                        .unwrap();
                let mut got = 0;
                let mut buf = vec![0u8; 16 * 1024];
                while got < read {
                    match exec::timeout(Duration::from_secs(5), tls.read(&mut buf)).await {
                        Ok(Ok(0)) | Ok(Err(_)) | Err(_) => break,
                        Ok(Ok(n)) => got += n,
                    }
                    // Slow reader.
                    exec::sleep(Duration::from_millis(1)).await;
                }
                // Hold the connection open without reading.
                exec::sleep(deadline.min(Duration::from_secs(2)) + Duration::from_millis(300))
                    .await;
                got
            };
            let ((ok, took), got) = futures::join!(serve, call);
            (ok, took, got)
        })
    }

    #[test]
    fn writes_complete_under_authority() {
        let (ok, _, got) = fenced(2 << 20, (2 << 20) + 4, Duration::from_secs(10), None, None);
        assert!(ok);
        assert_eq!(got, (2 << 20) + 4);
    }

    #[test]
    fn stalled_reader_is_cut_at_the_write_deadline() {
        let (ok, took, _) = fenced(32 << 20, 0, Duration::from_millis(500), None, None);
        assert!(!ok);
        assert!(took < Duration::from_secs(3), "{took:?}");
    }

    #[test]
    fn revocation_during_a_write_stops_it() {
        // The reader drains slowly; authority ends partway.
        let (ok, took, got) = fenced(
            64 << 20,
            usize::MAX,
            Duration::from_secs(30),
            Some(Duration::from_millis(100)),
            None,
        );
        assert!(!ok);
        assert!(got < (64 << 20) + 4, "{got}");
        assert!(took < Duration::from_secs(5), "{took:?}");
    }

    #[test]
    fn caller_expiry_stops_a_long_response() {
        // Seat authority stays fresh; the caller's credential expires.
        let (ok, took, got) = fenced(
            64 << 20,
            usize::MAX,
            Duration::from_secs(30),
            None,
            Some(Duration::from_millis(150)),
        );
        assert!(!ok);
        assert!(got < (64 << 20) + 4, "{got}");
        assert!(took < Duration::from_secs(5), "{took:?}");
        // Already expired: nothing, not even the head, is written.
        let (ok, _, got) = fenced(10, 14, Duration::from_secs(2), None, Some(Duration::ZERO));
        assert!(!ok);
        assert_eq!(got, 0);
    }

    #[test]
    fn fence_never_extends_either_deadline() {
        let now = Instant::now();
        let (a, b) = (now + Duration::from_secs(1), now + Duration::from_secs(2));
        assert_eq!(fence(Some(a), Some(b)), Some(a));
        assert_eq!(fence(Some(b), Some(a)), Some(a));
        assert_eq!(fence(Some(b), None), Some(b));
        assert_eq!(fence(None, Some(a)), None);
        assert_eq!(fence(None, None), None);
        assert!(caller_deadline(unix_now()) <= Instant::now());
        assert!(caller_deadline(0) <= Instant::now());
        let later = caller_deadline(unix_now() + 100);
        assert!(later > Instant::now() + Duration::from_secs(98));
    }

    #[test]
    fn unread_error_bodies_are_never_parsed_as_requests() {
        type Pipelined<'a> = (&'a str, &'a str, &'a [(&'a str, &'a str)], u16);
        fake::run(|_, kube| async move {
            let (shared, pin) = seated(kube);
            let listener = Listener::bind(format!("{POD_IP}:0").parse().unwrap()).unwrap();
            let address = listener.local_addr().unwrap();
            exec::spawn(Server::with_budget(shared.clone(), budget()).run(listener)).unwrap();
            // Each body is itself a complete request that would get a
            // different (401) answer if it were parsed as the next request.
            let smuggled = http::request("GET", "x", wire::KEYRING_PATH, &[], &[]);
            let json = [("Content-Type", "application/json")];
            let cases: [Pipelined<'_>; 3] = [
                ("POST", "/nope", &json, 400),
                ("POST", wire::BOOTSTRAP_PATH, &[], 400),
                ("GET", wire::SNAPSHOT_PATH, &[], 400),
            ];
            for (method, target, headers, want) in cases {
                let mut c = connect(address, &pin).await.unwrap();
                let mut out = http::request(method, "x", target, headers, &smuggled);
                // And a pipelined request after the body.
                out.extend_from_slice(&smuggled);
                c.tls.write_all(&out).await.unwrap();
                assert_eq!(response(&mut c).await.0, want, "{method} {target}");
                assert!(closed_without_reply(&mut c).await, "{method} {target}");
            }
            // Control: a bodiless error keeps the connection, so the
            // pipelined request is answered.
            let mut c = connect(address, &pin).await.unwrap();
            let mut out = http::request("GET", "x", "/nope", &[], &[]);
            out.extend_from_slice(&smuggled);
            c.tls.write_all(&out).await.unwrap();
            assert_eq!(response(&mut c).await.0, 400);
            assert_eq!(response(&mut c).await.0, 401);
            shared.stopping.set(true);
        });
    }

    fn post_head(path: &str, length: usize) -> Vec<u8> {
        format!(
            "POST {path} HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\nContent-Length: {length}\r\n\r\n"
        )
        .into_bytes()
    }

    #[test]
    fn bootstrap_admission_is_held_across_the_body_read() {
        fake::run(|_, kube| async move {
            let (shared, pin) = seated(kube);
            let listener = Listener::bind(format!("{POD_IP}:0").parse().unwrap()).unwrap();
            let address = listener.local_addr().unwrap();
            // One bootstrap slot; a long body deadline.
            let budget = Budget {
                io: Duration::from_millis(800),
                idle: Duration::from_secs(5),
                ..budget()
            };
            exec::spawn(Server::with_budget(shared.clone(), budget).run(listener)).unwrap();
            let mut a = connect(address, &pin).await.unwrap();
            a.tls
                .write_all(&post_head(wire::BOOTSTRAP_PATH, 100))
                .await
                .unwrap();
            a.tls.write_all(b"{").await.unwrap();
            exec::sleep(Duration::from_millis(100)).await;
            // A second enrollment is refused before its body is buffered.
            let mut b = connect(address, &pin).await.unwrap();
            let mut req = post_head(wire::BOOTSTRAP_PATH, 2);
            req.extend_from_slice(b"{}");
            b.tls.write_all(&req).await.unwrap();
            let (status, headers) = response(&mut b).await;
            assert_eq!(status, 429);
            assert!(headers.iter().any(|(k, v)| k == "Retry-After" && v == "1"));
            drop(b);
            // The stalled body times out and frees the slot.
            assert!(closed_without_reply(&mut a).await);
            let mut c = connect(address, &pin).await.unwrap();
            c.tls.write_all(&req).await.unwrap();
            // Admitted; the seat is not ready.
            assert_eq!(response(&mut c).await.0, 503);
            shared.stopping.set(true);
        });
    }

    /// A dual-stack Pod whose `status.podIP` is IPv4 behind an IPv6-primary
    /// Service: the seat publishes both Pod IPs, the primary picks the IPv6
    /// one for the IPv6 EndpointSlice, and the API server reaches the
    /// webhook there over TLS, trusting the published webhook CA and
    /// verifying the Service DNS name.
    #[test]
    fn webhook_serves_the_service_family_when_pod_ip_family_differs() {
        fake::run(|_, kube| async move {
            let mut shared = crate::shared::tests::shared(kube);
            Rc::get_mut(&mut shared).unwrap().config.webhook_alternate =
                Some("::1".parse().unwrap());
            let seat = crate::seat::new_seat(&shared, 0).unwrap();
            let ca = seat.webhook.borrow().ca_der.clone();
            *shared.seat.borrow_mut() = Some(Rc::new(seat));
            let published = crate::seat::webhook_addresses(&shared);
            assert_eq!(published, "127.0.0.2,::1");
            let ips = crate::primary::webhook_ips(Some(&published), POD_IP.parse().unwrap());
            let v6 = *ips.iter().find(|ip| ip.is_ipv6()).unwrap();
            let listener = Listener::bind(std::net::SocketAddr::new(v6, 0)).unwrap();
            let address = listener.local_addr().unwrap();
            spawn_webhooks(&shared, [listener]).unwrap();
            let mut roots = rustls::RootCertStore::empty();
            roots
                .add(rustls::pki_types::CertificateDer::from(ca))
                .unwrap();
            let tls = Arc::new(
                rustls::ClientConfig::builder_with_provider(Arc::new(
                    rustls::crypto::ring::default_provider(),
                ))
                .with_safe_default_protocol_versions()
                .unwrap()
                .with_root_certificates(roots)
                .with_no_client_auth(),
            );
            let name = ServerName::try_from("racer-webhook.racer-system.svc").unwrap();
            let tcp = Tcp::connect(address).await.unwrap();
            let mut conn = Conn::new(TlsStream::client(tcp, tls, name).await.unwrap());
            conn.tls.write_all(&post_head("/other", 0)).await.unwrap();
            assert_eq!(response(&mut conn).await.0, 404);
            shared.stopping.set(true);
        });
    }

    /// Bind one listener per family, all drawing on `budget`.
    fn dual_stack_webhooks(
        shared: &Rc<Shared>,
        budget: &WebhookBudget,
    ) -> [std::net::SocketAddr; 2] {
        [POD_IP, "::1"].map(|ip| {
            let ip: std::net::IpAddr = ip.parse().unwrap();
            let listener = Listener::bind(std::net::SocketAddr::new(ip, 0)).unwrap();
            let address = listener.local_addr().unwrap();
            exec::spawn(serve_webhook(
                shared.clone(),
                listener,
                budget.clone(),
                Duration::from_secs(5),
                Duration::from_millis(800),
            ))
            .unwrap();
            address
        })
    }

    /// A connection on one family uses the only connection slot of the
    /// shared budget; the other family gets none until it is released.
    #[test]
    fn webhook_connections_are_capped_across_families() {
        fake::run(|_, kube| async move {
            let (shared, pin) = seated(kube);
            let [v4, v6] = dual_stack_webhooks(&shared, &WebhookBudget::new(1, 1));
            let held = connect(v4, &pin).await.unwrap();
            exec::sleep(Duration::from_millis(100)).await;
            let refused = connect(v6, &pin).await;
            assert!(refused.is_err() || closed_without_reply(&mut refused.unwrap()).await);
            drop(held);
            exec::sleep(Duration::from_millis(100)).await;
            let mut c = connect(v6, &pin).await.unwrap();
            c.tls.write_all(&post_head("/other", 0)).await.unwrap();
            assert_eq!(response(&mut c).await.0, 404);
            shared.stopping.set(true);
        });
    }

    /// An IPv4 request holds the only body permit through its read; a
    /// complete IPv6 request is refused instead of buffered.
    #[test]
    fn webhook_body_permits_are_shared_across_families() {
        fake::run(|_, kube| async move {
            let (shared, pin) = seated(kube);
            let [v4, v6] = dual_stack_webhooks(&shared, &WebhookBudget::new(4, 1));
            let mut a = connect(v4, &pin).await.unwrap();
            a.tls
                .write_all(&post_head(racer_admission::PATH, 100))
                .await
                .unwrap();
            exec::sleep(Duration::from_millis(100)).await;
            let mut req = post_head(racer_admission::PATH, 2);
            req.extend_from_slice(b"{}");
            let mut b = connect(v6, &pin).await.unwrap();
            b.tls.write_all(&req).await.unwrap();
            assert_eq!(response(&mut b).await.0, 429);
            assert!(closed_without_reply(&mut a).await);
            // The permit returns to the shared budget.
            let mut c = connect(v6, &pin).await.unwrap();
            c.tls.write_all(&req).await.unwrap();
            assert_ne!(response(&mut c).await.0, 429);
            shared.stopping.set(true);
        });
    }

    #[test]
    fn webhook_body_permit_is_held_across_read() {
        fake::run(|_, kube| async move {
            let (shared, pin) = seated(kube);
            let listener = Listener::bind(format!("{POD_IP}:0").parse().unwrap()).unwrap();
            let address = listener.local_addr().unwrap();
            exec::spawn(run_webhook_with(
                shared.clone(),
                listener,
                1,
                Duration::from_secs(5),
                Duration::from_millis(800),
            ))
            .unwrap();
            let mut a = connect(address, &pin).await.unwrap();
            a.tls
                .write_all(&post_head(racer_admission::PATH, 100))
                .await
                .unwrap();
            exec::sleep(Duration::from_millis(100)).await;
            let mut req = post_head(racer_admission::PATH, 2);
            req.extend_from_slice(b"{}");
            let mut b = connect(address, &pin).await.unwrap();
            b.tls.write_all(&req).await.unwrap();
            assert_eq!(response(&mut b).await.0, 429);
            assert!(closed_without_reply(&mut a).await);
            let mut c = connect(address, &pin).await.unwrap();
            c.tls.write_all(&req).await.unwrap();
            assert_eq!(response(&mut c).await.0, 503);
            shared.stopping.set(true);
        });
    }

    #[test]
    fn write_slot_is_held_until_the_reply_is_dropped() {
        let slots = Slots::new(1);
        let r = Reply::ok(Arc::new(vec![1; 10]), slots.try_acquire().unwrap());
        assert!(slots.try_acquire().is_none());
        // The body is shared, not copied.
        let body = Arc::new(vec![0u8; 1 << 20]);
        let r2 = Reply::ok(body.clone(), Slots::new(1).try_acquire().unwrap());
        assert!(Arc::ptr_eq(&r2.body, &body));
        drop(r);
        assert!(slots.try_acquire().is_some());
    }

    type Case<'a> = (&'a str, String, &'a [(&'a str, &'a str)], u16);

    #[test]
    fn webhook_routes_strictly_and_bounds_requests() {
        fake::run(|_, kube| async move {
            let (shared, pin) = seated(kube);
            let listener = Listener::bind(format!("{POD_IP}:0").parse().unwrap()).unwrap();
            let address = listener.local_addr().unwrap();
            exec::spawn(run_webhook_with(
                shared.clone(),
                listener,
                8,
                Duration::from_millis(400),
                Duration::from_millis(400),
            ))
            .unwrap();
            let json = [("Content-Type", "application/json")];
            let cases: [Case<'_>; 13] = [
                ("POST", format!("{}?x=1", racer_admission::PATH), &json, 404),
                (
                    "POST",
                    format!("{}?timeout=5s&x=1", racer_admission::PATH),
                    &json,
                    404,
                ),
                (
                    "POST",
                    format!("{}?timeout=0s", racer_admission::PATH),
                    &json,
                    404,
                ),
                (
                    "POST",
                    format!("{}?timeout=31s", racer_admission::PATH),
                    &json,
                    404,
                ),
                (
                    "POST",
                    format!("{}?timeout=05s", racer_admission::PATH),
                    &json,
                    404,
                ),
                (
                    "POST",
                    format!("{}?timeout=5s&timeout=5s", racer_admission::PATH),
                    &json,
                    404,
                ),
                ("POST", "/other".into(), &json, 404),
                ("GET", racer_admission::PATH.into(), &json, 405),
                (
                    "POST",
                    racer_admission::PATH.into(),
                    &[("Content-Type", "text/plain")],
                    415,
                ),
                // Valid route, but no object image configured.
                ("POST", racer_admission::PATH.into(), &json, 503),
                (
                    "POST",
                    format!("{}?timeout=1s", racer_admission::PATH),
                    &json,
                    503,
                ),
                (
                    "POST",
                    format!("{}?timeout=5s", racer_admission::PATH),
                    &json,
                    503,
                ),
                (
                    "POST",
                    format!("{}?timeout=30s", racer_admission::PATH),
                    &json,
                    503,
                ),
            ];
            for (method, target, headers, want) in cases {
                let mut c = connect(address, &pin).await.unwrap();
                c.tls
                    .write_all(&http::request(method, "x", &target, headers, b"{}"))
                    .await
                    .unwrap();
                assert_eq!(response(&mut c).await.0, want, "{method} {target}");
            }
            // Oversized declared body: 413 before reading it.
            let mut c = connect(address, &pin).await.unwrap();
            let big = (racer_admission::MAX_REQUEST_BYTES + 1).to_string();
            c.tls
                .write_all(
                    format!(
                        "POST {} HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\nContent-Length: {big}\r\n\r\n",
                        racer_admission::PATH
                    )
                    .as_bytes(),
                )
                .await
                .unwrap();
            assert_eq!(response(&mut c).await.0, 413);
            // Slow body is dropped.
            let mut c = connect(address, &pin).await.unwrap();
            c.tls
                .write_all(
                    format!(
                        "POST {} HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\nContent-Length: 10\r\n\r\n{{",
                        racer_admission::PATH
                    )
                    .as_bytes(),
                )
                .await
                .unwrap();
            assert!(closed_without_reply(&mut c).await);
            shared.stopping.set(true);
        });
    }

    #[test]
    fn cursors() {
        assert_eq!(cursor(None), Ok(None));
        assert_eq!(cursor(Some("after=5")), Ok(Some(5)));
        assert_eq!(cursor(Some("after=0")), Err(ProtocolFailure::Conflict));
        for bad in [
            "after=05",
            "after=",
            "x=1",
            "after=1&x=2",
            "after=-1",
            "after=+1",
        ] {
            assert_eq!(
                cursor(Some(bad)),
                Err(ProtocolFailure::InvalidRequest),
                "{bad}"
            );
        }
    }

    #[test]
    fn error_mapping() {
        let r = String::from_utf8(error_response(ProtocolFailure::Overloaded)).unwrap();
        assert!(r.starts_with("HTTP/1.1 429"));
        assert!(r.contains("Retry-After: 1"));
        let r = String::from_utf8(error_response(ProtocolFailure::Forbidden)).unwrap();
        assert!(r.starts_with("HTTP/1.1 403") && !r.contains("Retry-After"));
        let body = r.split("\r\n\r\n").nth(1).unwrap();
        assert_eq!(
            wire::decode_error(body.as_bytes()).unwrap().code,
            ProtocolFailure::Forbidden
        );
    }

    #[test]
    fn block_device_validation() {
        let mut a = std::collections::BTreeMap::new();
        assert_eq!(block_devices(&a), None);
        a.insert(
            crate::annotations::BLOCK_DEVICES.to_owned(),
            "nvme-.*".to_owned(),
        );
        assert_eq!(block_devices(&a).as_deref(), Some("nvme-.*"));
        a.insert(crate::annotations::BLOCK_DEVICES.to_owned(), "(".to_owned());
        assert_eq!(block_devices(&a), None);
        a.insert(
            crate::annotations::BLOCK_DEVICES.to_owned(),
            "a".repeat(1025),
        );
        assert_eq!(block_devices(&a), None);
    }

    #[test]
    fn long_poll_replies_queue_for_a_write_slot() {
        fake::run(|_, kube| async move {
            let (shared, _) = seated(kube);
            let server = Server::with_budget(shared.clone(), budget());
            let expires = unix_now() + 3600;
            let held = server.write_slot().unwrap();
            // Enrollment and other non-poll replies still fail fast.
            assert_eq!(server.write_slot().err(), Some(ProtocolFailure::Overloaded));
            // A poll whose deadline (plus grace) passes while queued: 429.
            let past = Instant::now() - limits::WRITE_QUEUE_GRACE;
            assert_eq!(
                server.queued_write_slot(past, expires).await.err(),
                Some(ProtocolFailure::Overloaded)
            );
            // An expired credential bounds the wait too.
            let now = Instant::now();
            assert_eq!(
                server.queued_write_slot(now, unix_now()).await.err(),
                Some(ProtocolFailure::Overloaded)
            );
            // A queued poll gets the slot when it is released instead of a
            // 429, then rechecks readiness (this seat is not ready: 503).
            let result = Rc::new(RefCell::new(None));
            let (s, r) = (server.clone(), result.clone());
            exec::spawn(async move {
                let out = s.queued_write_slot(Instant::now(), expires).await;
                *r.borrow_mut() = Some(out.err());
            })
            .unwrap();
            exec::sleep(Duration::from_millis(20)).await;
            assert!(result.borrow().is_none());
            drop(held);
            exec::sleep(Duration::from_millis(20)).await;
            assert_eq!(*result.borrow(), Some(Some(ProtocolFailure::Unavailable)));
            assert!(server.write_slot().is_ok());
            shared.stopping.set(true);
        });
    }

    #[test]
    fn replica_requires_the_primary_term() {
        assert_eq!(require_primary(true), Ok(()));
        assert_eq!(require_primary(false), Err(ProtocolFailure::Unavailable));
    }

    #[test]
    fn bearer_keyring_rechecks_only_after_a_wait() {
        let ttl = crate::step::authz::CALLER_TTL;
        let zero = Duration::ZERO;
        assert!(bearer_recheck(true, false, zero));
        assert!(!bearer_recheck(true, true, zero));
        assert!(!bearer_recheck(true, true, ttl - Duration::from_millis(1)));
        // A write slot wait that reaches the cache TTL forces a recheck.
        assert!(bearer_recheck(true, true, ttl));
        assert!(bearer_recheck(true, true, ttl * 3));
        assert!(!bearer_recheck(false, false, zero));
        assert!(!bearer_recheck(false, true, ttl * 3));
    }

    #[test]
    fn poll_admission_is_per_node() {
        let set = RefCell::new(HashSet::new());
        let g = admit_poll(&set, "n", None, Some(1)).unwrap();
        assert_eq!(
            admit_poll(&set, "n", None, Some(1)).err(),
            Some(ProtocolFailure::Overloaded)
        );
        assert!(admit_poll(&set, "m", None, Some(1)).is_ok());
        drop(g);
        assert!(admit_poll(&set, "n", None, Some(1)).is_ok());
    }
}
