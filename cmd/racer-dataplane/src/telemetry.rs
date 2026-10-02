//! Bounded diagnostics and HTTP endpoints, polled by an existing worker.
pub mod failures;
pub mod health {
    use crate::error::{Error, Result};
    use std::{
        sync::{Arc, Mutex},
        time::Instant,
    };
    #[derive(Clone, Default)]
    pub struct Health(Arc<Mutex<Status>>);
    #[derive(Default)]
    struct Status {
        lifecycle: State,
        resources: Resources,
    }
    #[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
    pub enum State {
        #[default]
        Starting,
        Ready,
        Degraded,
        Draining,
        Stopped,
    }
    /// Complete worker observation; no identities or credentials enter health state.
    #[derive(Clone, Copy, Debug, Default)]
    pub struct Resources {
        pub workers_usable: bool,
        pub storage_usable: bool,
        pub listeners_usable: bool,
        pub membership_usable: bool,
        pub admission_usable: bool,
        pub credentials_valid_until: Option<Instant>,
        pub observed_until: Option<Instant>,
    }
    impl Resources {
        pub fn usable_at(&self, now: Instant) -> bool {
            self.workers_usable
                && self.storage_usable
                && self.listeners_usable
                && self.membership_usable
                && self.admission_usable
                && self
                    .credentials_valid_until
                    .is_some_and(|expiry| now < expiry)
                && self.observed_until.is_some_and(|expiry| now < expiry)
        }
    }
    impl Health {
        pub fn state(&self) -> Result<State> {
            self.state_at(crate::runtime::environment::now())
        }
        pub fn state_at(&self, now: Instant) -> Result<State> {
            let status = self.0.lock().map_err(|_| Error::Unavailable)?;
            Ok(match status.lifecycle {
                State::Ready if !status.resources.usable_at(now) => State::Degraded,
                state => state,
            })
        }
        pub fn observe(&self, resources: Resources) -> Result<()> {
            self.0.lock().map_err(|_| Error::Unavailable)?.resources = resources;
            Ok(())
        }
        pub fn ready(&self) -> bool {
            self.state().is_ok_and(|state| state == State::Ready)
        }
        /// Starting, degraded, and draining remain live; a response establishes progress.
        pub fn live(&self) -> bool {
            self.state().is_ok_and(|state| state != State::Stopped)
        }
        pub fn transition(&self, state: State) -> Result<()> {
            let mut status = self.0.lock().map_err(|_| Error::Unavailable)?;
            if status.lifecycle == State::Stopped && state != State::Stopped
                || status.lifecycle == State::Draining
                    && !matches!(state, State::Draining | State::Stopped)
                || state == State::Ready
                    && !status
                        .resources
                        .usable_at(crate::runtime::environment::now())
            {
                return Err(Error::Unavailable);
            }
            status.lifecycle = state;
            Ok(())
        }
    }
    #[cfg(test)]
    mod tests {
        use super::*;
        use std::time::Duration;
        #[test]
        fn observation_and_credential_expiry_are_inclusive() {
            let now = crate::runtime::environment::now();
            for (observed, boundary) in [(5, 5), (20, 10)] {
                let health = Health::default();
                health
                    .observe(Resources {
                        workers_usable: true,
                        storage_usable: true,
                        listeners_usable: true,
                        membership_usable: true,
                        admission_usable: true,
                        credentials_valid_until: Some(now + Duration::from_secs(10)),
                        observed_until: Some(now + Duration::from_secs(observed)),
                    })
                    .unwrap();
                health.transition(State::Ready).unwrap();
                assert_eq!(
                    health
                        .state_at(now + Duration::from_secs(boundary) - Duration::from_nanos(1))
                        .unwrap(),
                    State::Ready
                );
                assert_eq!(
                    health
                        .state_at(now + Duration::from_secs(boundary))
                        .unwrap(),
                    State::Degraded
                );
            }
        }
    }
}
pub mod metrics;
pub mod send_crc {
    //! Opt-in HTTP send fingerprints, never authentication or payload scans on I/O.
    //! Shared limits: one active owner, two samples/second, 240 total, 120 seconds.
    //! Cached CRCs are reused. Pending/unavailable samples do not prove agreement.
    use crate::{
        error::{Error, Result},
        model::NodeId,
        runtime::environment,
        telemetry::failures::AeadFailure,
    };
    use std::{
        sync::{Arc, Mutex},
        time::{Duration, Instant},
    };
    pub const CAPACITY: usize = 64;
    #[derive(Clone, Debug)]
    pub struct Pair {
        pub sender: NodeId,
        pub receiver: NodeId,
    }
    impl Pair {
        pub fn parse(value: &str) -> Result<Self> {
            let (sender, receiver) = value.split_once(',').ok_or(Error::InvalidConfiguration)?;
            let pair = Self {
                sender: NodeId(sender.into()),
                receiver: NodeId(receiver.into()),
            };
            pair.validate()?;
            Ok(pair)
        }
        pub fn validate(&self) -> Result<()> {
            if self.sender == self.receiver
                || !crate::security::identity::canonical_uuid(&self.sender.0)
                || !crate::security::identity::canonical_uuid(&self.receiver.0)
            {
                return Err(Error::InvalidConfiguration);
            }
            Ok(())
        }
    }
    #[derive(Clone, Default)]
    pub struct Samples(Arc<Mutex<State>>);
    struct State {
        first: Option<Instant>,
        last: Option<Instant>,
        busy: bool,
        eligible: u64,
        sampled: u64,
        skipped: u64,
        entries: [Option<Arc<Sample>>; CAPACITY],
    }
    impl Default for State {
        fn default() -> Self {
            Self {
                first: None,
                last: None,
                busy: false,
                eligible: 0,
                sampled: 0,
                skipped: 0,
                entries: std::array::from_fn(|_| None),
            }
        }
    }
    struct Sample {
        sequence: u64,
        sender: NodeId,
        receiver: NodeId,
        data: Mutex<Data>,
    }
    struct Data {
        crypto: Option<crate::runtime::crypto::CryptoId>,
        send: &'static str,
        status: &'static str,
        error: Option<Error>,
        facts: Option<AeadFailure>,
    }
    pub(crate) struct Ticket(Arc<Sample>);
    pub(crate) struct Work {
        sample: Arc<Sample>,
        owner: Samples,
        pub facts: Option<AeadFailure>,
        pub cached: bool,
    }
    impl Samples {
        #[cfg(test)]
        pub(crate) fn fill_test_ring(&self) {
            let pair = Pair::parse(
                "8816d91d-e896-49bf-ba8a-da97ede93818,11111111-1111-4111-8111-111111111111",
            )
            .unwrap();
            for _ in 0..CAPACITY {
                self.0.lock().unwrap().last = None;
                let (ticket, mut work) = self.begin(&pair, &pair.sender, &pair.receiver).unwrap();
                work.facts = Some(crate::telemetry::failures::test_aead_failure());
                work.identify(crate::runtime::crypto::CryptoId {
                    worker: crate::model::WorkerId(u16::MAX),
                    generation: u64::MAX,
                    sequence: u64::MAX,
                });
                work.finish(None);
                ticket.finish(true);
            }
        }
        pub(crate) fn begin(
            &self,
            pair: &Pair,
            sender: &NodeId,
            receiver: &NodeId,
        ) -> Option<(Ticket, Work)> {
            if sender != &pair.sender || receiver != &pair.receiver {
                return None;
            }
            let now = environment::now();
            let mut state = self.0.lock().unwrap_or_else(|e| e.into_inner());
            state.eligible = state.eligible.saturating_add(1);
            let first = *state.first.get_or_insert(now);
            if state.busy
                || state.sampled >= 240
                || now.saturating_duration_since(first) >= Duration::from_secs(120)
                || state.last.is_some_and(|last| {
                    now.saturating_duration_since(last) < Duration::from_millis(500)
                })
            {
                state.skipped = state.skipped.saturating_add(1);
                return None;
            }
            state.busy = true;
            state.last = Some(now);
            state.sampled += 1;
            let sample = Arc::new(Sample {
                sequence: state.sampled,
                sender: sender.clone(),
                receiver: receiver.clone(),
                data: Mutex::new(Data {
                    crypto: None,
                    send: "pending",
                    status: "pending",
                    error: None,
                    facts: None,
                }),
            });
            let index = (state.sampled as usize - 1) % CAPACITY;
            state.entries[index] = Some(sample.clone());
            Some((
                Ticket(sample.clone()),
                Work {
                    sample,
                    owner: self.clone(),
                    facts: None,
                    cached: false,
                },
            ))
        }
        pub fn write(&self, out: &mut impl std::fmt::Write) -> std::fmt::Result {
            let (entries, eligible, sampled, skipped, busy) = {
                let state = self.0.lock().unwrap_or_else(|e| e.into_inner());
                (
                    state.entries.clone(),
                    state.eligible,
                    state.sampled,
                    state.skipped,
                    state.busy,
                )
            };
            writeln!(
                out,
                "eligible={eligible} sampled={sampled} skipped={skipped} busy={} retained={} overwritten={} capacity={CAPACITY}",
                u8::from(busy),
                sampled.min(CAPACITY as u64),
                sampled.saturating_sub(CAPACITY as u64)
            )?;
            for sequence in sampled.saturating_sub(CAPACITY as u64) + 1..=sampled {
                if let Some(sample) = &entries[(sequence as usize - 1) % CAPACITY] {
                    let data = sample.data.lock().unwrap_or_else(|e| e.into_inner());
                    write!(
                        out,
                        "seq={} sender={} receiver={} send={} status={} error={:?}",
                        sample.sequence,
                        sample.sender.0,
                        sample.receiver.0,
                        data.send,
                        data.status,
                        data.error
                    )?;
                    if let Some(id) = data.crypto {
                        write!(
                            out,
                            " w={} crypto={}:{}",
                            id.worker.0, id.generation, id.sequence
                        )?;
                    }
                    if let Some(f) = data.facts {
                        f.write_fields(out)?;
                    }
                    writeln!(out)?;
                }
            }
            Ok(())
        }
    }
    impl Ticket {
        pub(crate) fn finish(&self, success: bool) {
            self.0.data.lock().unwrap_or_else(|e| e.into_inner()).send =
                if success { "completed" } else { "failed" };
        }
    }
    impl Drop for Ticket {
        fn drop(&mut self) {
            let mut data = self.0.data.lock().unwrap_or_else(|e| e.into_inner());
            if data.send == "pending" {
                data.send = "abandoned";
            }
        }
    }
    impl Work {
        pub(crate) fn identify(&self, id: crate::runtime::crypto::CryptoId) {
            self.sample
                .data
                .lock()
                .unwrap_or_else(|e| e.into_inner())
                .crypto = Some(id);
        }
        pub(crate) fn finish(&self, error: Option<Error>) {
            let mut data = self.sample.data.lock().unwrap_or_else(|e| e.into_inner());
            data.facts = self.facts;
            data.error = error;
            data.status = if error.is_some() {
                "unavailable"
            } else if self.cached {
                "cached"
            } else {
                "computed"
            };
        }
    }
    impl Drop for Work {
        fn drop(&mut self) {
            {
                let mut data = self.sample.data.lock().unwrap_or_else(|e| e.into_inner());
                if data.status == "pending" {
                    data.status = "unavailable";
                }
            }
            self.owner.0.lock().unwrap_or_else(|e| e.into_inner()).busy = false;
        }
    }
    #[cfg(test)]
    mod tests {
        use super::*;
        fn pair() -> Pair {
            Pair::parse("8816d91d-e896-49bf-ba8a-da97ede93818,11111111-1111-4111-8111-111111111111")
                .unwrap()
        }
        #[test]
        fn send_crc_pair_and_shared_owner_bounds() {
            for value in [
                "",
                "x,y",
                "11111111-1111-4111-8111-111111111111,11111111-1111-4111-8111-111111111111",
                "x,y,z",
            ] {
                assert!(Pair::parse(value).is_err());
            }
            let p = pair();
            let samples = Samples::default();
            assert!(samples.begin(&p, &p.receiver, &p.sender).is_none());
            let (ticket, work) = samples.begin(&p, &p.sender, &p.receiver).unwrap();
            drop(ticket);
            let other = samples.clone();
            let p2 = p.clone();
            assert!(
                std::thread::spawn(move || other.begin(&p2, &p2.sender, &p2.receiver).is_none())
                    .join()
                    .unwrap()
            );
            assert!(samples.0.lock().unwrap().busy);
            drop(work);
            assert!(!samples.0.lock().unwrap().busy);
            assert!(
                samples.begin(&p, &p.sender, &p.receiver).is_none(),
                "burst is one"
            );
            let mut text = String::new();
            samples.write(&mut text).unwrap();
            assert!(text.contains("send=abandoned status=unavailable"));
            samples.0.lock().unwrap().last = None;
            samples.0.lock().unwrap().first = Some(environment::now() - Duration::from_secs(120));
            assert!(samples.begin(&p, &p.sender, &p.receiver).is_none());
        }
        #[test]
        fn send_crc_ring_is_bounded_and_preserves_send_outcomes() {
            let p = pair();
            let samples = Samples::default();
            for i in 0..240 {
                samples.0.lock().unwrap().last = None;
                let (ticket, mut work) = samples.begin(&p, &p.sender, &p.receiver).unwrap();
                ticket.finish(i % 2 == 0);
                work.facts = Some(crate::telemetry::failures::test_aead_failure());
                work.finish(None);
                drop(work);
                drop(ticket);
            }
            samples.0.lock().unwrap().last = None;
            assert!(samples.begin(&p, &p.sender, &p.receiver).is_none());
            let mut text = String::new();
            samples.write(&mut text).unwrap();
            assert_eq!(text.lines().count(), CAPACITY + 1);
            assert!(text.contains("overwritten=176 capacity=64\nseq=177 "));
            assert!(text.contains("send=completed"));
            assert!(text.contains("send=failed"));
            assert!(text.len() < crate::telemetry::MAX_RESPONSE_BYTES - 256);
        }
    }
}

use crate::runtime::reactor::Descriptor;
use crate::{
    error::{Error, Operation, Result},
    model::ResourceClass,
    runtime::{
        admission::{Admission, Reservation},
        deadline::{Deadline, RequestScope},
        reactor::{IoBuffer, Reactor},
    },
};
use metrics::{Event, Gauge, GaugeLease};
#[cfg(test)]
use std::net::TcpListener;
#[cfg(test)]
use std::time::Instant;
use std::{
    cell::{Cell, OnceCell},
    fmt::Write,
    net::SocketAddr,
    rc::Rc,
    task::Poll,
    time::Duration,
};
use zeroize::Zeroize;

#[derive(Default)]
pub struct Telemetry {
    pub(crate) membership: OnceCell<Rc<dyn Fn() -> Result<MembershipDiagnostic>>>,
    pub send_crc: send_crc::Samples,
    pub failures: failures::Failures,
    pub metrics: metrics::Metrics,
    pub health: health::Health,
    io: OnceCell<Rc<DiagnosticIo>>,
}

/// Fixed-size diagnostic, with exact decimal counters and no metric labels.
#[derive(Default)]
pub(crate) struct MembershipDiagnostic {
    pub accepted_sequence: u64,
    pub accepted_membership: u64,
    pub accepted_hash: [u8; 32],
    pub pending_sequence: u64,
    pub pending_membership: u64,
    pub expected_workers: usize,
    pub matching_workers: usize,
}
impl MembershipDiagnostic {
    pub(crate) fn fully_applied(&self) -> bool {
        self.accepted_sequence != 0
            && self.pending_sequence == 0
            && self.expected_workers != 0
            && self.matching_workers == self.expected_workers
    }
    fn write(&self, out: &mut impl Write) -> std::fmt::Result {
        write!(
            out,
            "accepted_sequence={} accepted_membership={} accepted_membership_hash=",
            self.accepted_sequence, self.accepted_membership
        )?;
        for byte in self.accepted_hash {
            write!(out, "{byte:02x}")?;
        }
        writeln!(
            out,
            " pending_sequence={} pending_membership={} expected_workers={} matching_workers={} fully_applied={}",
            self.pending_sequence,
            self.pending_membership,
            self.expected_workers,
            self.matching_workers,
            u8::from(self.fully_applied())
        )
    }
}

impl Telemetry {
    /// Reserve diagnostic memory/control slots before data admission. Does not
    /// bind a listener or spawn work. Duplicate attachment is rejected.
    pub fn attach_io(&self, reactor: Rc<Reactor>, admission: Rc<Admission>) -> Result<()> {
        if self.io.get().is_some() {
            return Err(Error::InvalidConfiguration);
        }
        let io = Rc::new(DiagnosticIo::attach(reactor, admission)?);
        self.io.set(io).map_err(|_| Error::InvalidConfiguration)
    }

    /// Poll alongside the worker reactor. Bind errors occur on first poll.
    /// After cancellation, drain the reactor before dropping attached resources.
    pub fn serve<'a>(&'a self, address: SocketAddr, scope: &'a RequestScope) -> Operation<'a, ()> {
        Box::pin(async move {
            let io = self.io.get().ok_or(Error::InvalidConfiguration)?.clone();
            self.serve_with_io(address, io, scope).await
        })
    }

    /// Alternative for integrators that retain attachment separately.
    pub fn serve_with_io<'a>(
        &'a self,
        address: SocketAddr,
        io: Rc<DiagnosticIo>,
        scope: &'a RequestScope,
    ) -> Operation<'a, ()> {
        Box::pin(async move {
            scope.check()?;
            let listener = Descriptor::tcp_listener(address)?;
            serve(self, listener, io, scope).await
        })
    }

    /// Transfer a listener, including port-zero bindings, to the worker reactor.
    pub fn serve_listener_with_io<'a>(
        &'a self,
        listener: std::net::TcpListener,
        io: Rc<DiagnosticIo>,
        scope: &'a RequestScope,
    ) -> Operation<'a, ()> {
        serve(self, listener.into(), io, scope)
    }
}

pub const MAX_CONNECTIONS: usize = 4;
pub const MAX_REQUEST_BYTES: usize = 1024;
pub const MAX_RESPONSE_BYTES: usize = 64 * 1024;
pub const CONNECTION_TIMEOUT: Duration = Duration::from_secs(2);
/// Fixed startup charge covers buffers and bounded service/future bookkeeping.
pub const RESERVED_BYTES: usize = (MAX_CONNECTIONS + 1) * (MAX_RESPONSE_BYTES + 4096);
pub const CONTROL_SLOTS: usize = MAX_CONNECTIONS + 1;

/// One attachment serves at most one listener. Resource reservations outlive
/// abandoned connection futures because the reactor retains their buffers.
pub struct DiagnosticIo {
    reactor: Rc<Reactor>,
    admission: Rc<Admission>,
    resources: Rc<Resources>,
    serving: Cell<bool>,
}
struct Resources {
    _memory: Reservation,
    submissions: Rc<crate::runtime::reactor::SubmissionCapacity>,
    active: Cell<usize>,
}
impl DiagnosticIo {
    /// Explicit startup acquisition, using the worker's already-budgeted reactor.
    /// Must run before ordinary request admission fills the memory quota.
    pub fn attach(reactor: Rc<Reactor>, admission: Rc<Admission>) -> Result<Self> {
        let control = admission.reserve(None, ResourceClass::ControlProgress, CONTROL_SLOTS)?;
        let mut memory = admission.reserve(None, ResourceClass::RequestContext, RESERVED_BYTES)?;
        let bookkeeping =
            memory.split(CONTROL_SLOTS * crate::runtime::reactor::SUBMISSION_BYTES)?;
        let submissions = reactor.reserve_submissions(control, bookkeeping)?;
        Ok(Self {
            reactor,
            admission,
            resources: Rc::new(Resources {
                _memory: memory,
                submissions,
                active: Cell::new(0),
            }),
            serving: Cell::new(false),
        })
    }
    fn buffer(&self, metrics: &metrics::Metrics) -> Result<Buffer> {
        if self.resources.active.get() >= MAX_CONNECTIONS {
            return Err(Error::Overloaded);
        }
        let gauge = metrics.lease(Gauge::DiagnosticConnections)?;
        let bytes = vec![0; MAX_RESPONSE_BYTES];
        self.resources.active.set(self.resources.active.get() + 1);
        Ok(Buffer {
            bytes,
            start: 0,
            end: MAX_REQUEST_BYTES,
            resources: self.resources.clone(),
            _gauge: gauge,
        })
    }
}
struct Serving(Rc<DiagnosticIo>);
impl Drop for Serving {
    fn drop(&mut self) {
        self.0.serving.set(false);
    }
}
struct Buffer {
    bytes: Vec<u8>,
    start: usize,
    end: usize,
    resources: Rc<Resources>,
    _gauge: GaugeLease,
}
impl Drop for Buffer {
    fn drop(&mut self) {
        self.bytes.as_mut_slice().zeroize();
        self.resources.active.set(self.resources.active.get() - 1);
    }
}
// SAFETY: private fixed Vec retains its reservation and is not aliased.
unsafe impl IoBuffer for Buffer {
    type Error = Error;
    fn bytes(&self) -> Result<&[u8]> {
        Ok(&self.bytes[self.start..self.end])
    }
    fn bytes_mut(&mut self) -> Result<&mut [u8]> {
        Ok(&mut self.bytes[self.start..self.end])
    }
}

fn serve<'a>(
    telemetry: &'a Telemetry,
    listener: Descriptor,
    io: Rc<DiagnosticIo>,
    scope: &'a RequestScope,
) -> Operation<'a, ()> {
    Box::pin(async move {
        scope.check()?;
        if io.serving.replace(true) {
            return Err(Error::InvalidConfiguration);
        }
        let _serving = Serving(io.clone());
        listener.set_nonblocking()?;
        let listener = Rc::new(listener);
        let mut accepting = None;
        let mut connections: Vec<Operation<'_, ()>> = Vec::with_capacity(MAX_CONNECTIONS);
        let cancellation = scope.cancellation.subscribe()?;
        std::future::poll_fn(|cx| {
            cancellation.register(cx.waker());
            scope.check()?;
            // Every poll performs bounded work. Slow headers cannot monopolize
            // the listener; up to four independent two-second exchanges progress.
            let mut index = 0;
            while index < connections.len() {
                match connections[index].as_mut().poll(cx) {
                    Poll::Pending => index += 1,
                    Poll::Ready(result) => {
                        drop(connections.swap_remove(index));
                        if let Err(error) = result {
                            telemetry.metrics.record(
                                match error {
                                    Error::DeadlineExceeded => Event::DiagnosticTimeout,
                                    _ => Event::DiagnosticIoError,
                                },
                                1,
                            )?;
                        }
                    }
                }
            }
            if accepting.is_none() && io.resources.active.get() < MAX_CONNECTIONS {
                accepting = Some(crate::runtime::retry_listener(scope, || {
                    io.reactor.accept_reserved(
                        listener.clone(),
                        Some(io.resources.submissions.clone()),
                        scope,
                    )
                }));
            }
            if let Some(accept) = &mut accepting {
                match accept.as_mut().poll(cx) {
                    Poll::Pending => (),
                    Poll::Ready(result) => {
                        accepting = None;
                        let fd = result?;
                        telemetry.metrics.record(Event::DiagnosticAccepted, 1)?;
                        let buffer = io.buffer(&telemetry.metrics)?;
                        connections.push(exchange(
                            telemetry,
                            io.clone(),
                            Rc::new(fd),
                            buffer,
                            scope,
                        ));
                        cx.waker().wake_by_ref();
                    }
                }
            }
            Poll::Pending
        })
        .await
    })
}

fn exchange<'a>(
    telemetry: &'a Telemetry,
    io: Rc<DiagnosticIo>,
    fd: Rc<Descriptor>,
    mut buffer: Buffer,
    parent: &'a RequestScope,
) -> Operation<'a, ()> {
    Box::pin(async move {
        let scope = RequestScope {
            body_deadlines: parent.body_deadlines,
            request: parent.request,
            deadline: Deadline(
                parent
                    .deadline
                    .0
                    .min(crate::runtime::environment::now() + CONNECTION_TIMEOUT),
            ),
            cancellation: parent.cancellation.clone(),
        };
        let mut used = 0;
        let route = loop {
            buffer.start = used;
            buffer.end = MAX_REQUEST_BYTES;
            let completed = io
                .reactor
                .recv_reserved(fd.clone(), buffer, io.resources.submissions.clone(), &scope)
                .await?;
            if completed.bytes == 0 || completed.bytes > MAX_REQUEST_BYTES - used {
                return Err(Error::Io);
            }
            used += completed.bytes;
            buffer = completed.buffer;
            if let Some(end) = buffer.bytes[..used]
                .windows(4)
                .position(|part| part == b"\r\n\r\n")
            {
                break parse(&buffer.bytes[..end + 4]);
            }
            if used == MAX_REQUEST_BYTES {
                break Route::TooLarge;
            }
        };
        // Headers are never retained in decoded objects, traces, or responses.
        buffer.bytes.as_mut_slice().zeroize();
        let length = respond(
            telemetry,
            route,
            !io.admission.is_stopped(),
            &mut buffer.bytes[..],
        )?;
        let mut sent = 0;
        while sent < length {
            buffer.start = sent;
            buffer.end = length;
            let completed = io
                .reactor
                .send_reserved(fd.clone(), buffer, io.resources.submissions.clone(), &scope)
                .await?;
            if completed.bytes == 0 || completed.bytes > length - sent {
                return Err(Error::Io);
            }
            sent += completed.bytes;
            buffer = completed.buffer;
        }
        // Always close. There is no pipelining, keepalive, body buffering, or echo.
        Ok(())
    })
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum Route {
    Membership,
    Health,
    Ready,
    Metrics,
    Failures,
    Aead,
    SendCrc,
    NotFound,
    Method,
    BadRequest,
    TooLarge,
}
fn parse(bytes: &[u8]) -> Route {
    let mut headers = [httparse::EMPTY_HEADER; 16];
    let mut request = httparse::Request::new(&mut headers);
    if !matches!(request.parse(bytes), Ok(httparse::Status::Complete(_)))
        || request.version != Some(1)
    {
        return Route::BadRequest;
    }
    let mut host = false;
    let mut length = false;
    for header in request.headers.iter() {
        if header.name.eq_ignore_ascii_case("host") {
            if host || header.value.is_empty() {
                return Route::BadRequest;
            }
            host = true;
        }
        if header.name.eq_ignore_ascii_case("transfer-encoding") {
            return Route::BadRequest;
        }
        if header.name.eq_ignore_ascii_case("content-length") {
            if length || header.value != b"0" {
                return Route::BadRequest;
            }
            length = true;
        }
    }
    if !host {
        return Route::BadRequest;
    }
    if request.method != Some("GET") {
        return Route::Method;
    }
    match request.path {
        Some("/debug/membership") => Route::Membership,
        Some("/healthz") => Route::Health,
        Some("/readyz") => Route::Ready,
        Some("/metrics") => Route::Metrics,
        Some("/debug/failures") => Route::Failures,
        Some("/debug/aead") => Route::Aead,
        Some("/debug/send-crc") => Route::SendCrc,
        _ => Route::NotFound,
    }
}

/// A fallible fixed-slice writer avoids allocating intermediate response strings.
struct Output<'a> {
    bytes: &'a mut [u8],
    used: usize,
}
impl Write for Output<'_> {
    fn write_str(&mut self, text: &str) -> std::fmt::Result {
        let end = self.used.checked_add(text.len()).ok_or(std::fmt::Error)?;
        self.bytes
            .get_mut(self.used..end)
            .ok_or(std::fmt::Error)?
            .copy_from_slice(text.as_bytes());
        self.used = end;
        Ok(())
    }
}
fn respond(
    telemetry: &Telemetry,
    route: Route,
    admission_usable: bool,
    bytes: &mut [u8],
) -> Result<usize> {
    let ready = admission_usable && telemetry.health.ready();
    let (status, body, event) = match route {
        Route::Health if telemetry.health.live() => ("200 OK", "ok\n", Event::DiagnosticHealth),
        Route::Health => (
            "503 Service Unavailable",
            "not live\n",
            Event::DiagnosticHealth,
        ),
        Route::Ready if ready => ("200 OK", "ready\n", Event::DiagnosticReady),
        Route::Ready => (
            "503 Service Unavailable",
            "not ready\n",
            Event::DiagnosticReady,
        ),
        Route::Metrics => ("200 OK", "", Event::DiagnosticMetrics),
        Route::Membership => ("200 OK", "", Event::DiagnosticMetrics),
        Route::Failures => ("200 OK", "", Event::DiagnosticFailures),
        Route::Aead => ("200 OK", "", Event::DiagnosticFailures),
        Route::SendCrc => ("200 OK", "", Event::DiagnosticFailures),
        Route::Method => (
            "405 Method Not Allowed",
            "method not allowed\n",
            Event::DiagnosticRejected,
        ),
        Route::NotFound => ("404 Not Found", "not found\n", Event::DiagnosticRejected),
        Route::BadRequest => (
            "400 Bad Request",
            "bad request\n",
            Event::DiagnosticRejected,
        ),
        Route::TooLarge => (
            "431 Request Header Fields Too Large",
            "headers too large\n",
            Event::DiagnosticRejected,
        ),
    };
    telemetry.metrics.record(event, 1)?;
    // Body starts after fixed header headroom, then moves next to the actual head.
    const HEAD: usize = 256;
    let (header, body_bytes) = bytes.split_at_mut(HEAD);
    let mut output = Output {
        bytes: body_bytes,
        used: 0,
    };
    if route == Route::Metrics {
        telemetry
            .metrics
            .write_prometheus(&mut output)
            .map_err(|_| Error::Internal)?;
        writeln!(
            output,
            "# TYPE racer_ready gauge\nracer_ready {}\n# TYPE racer_live gauge\nracer_live {}",
            u8::from(ready),
            u8::from(telemetry.health.live())
        )
        .map_err(|_| Error::Internal)?;
    } else if route == Route::Membership {
        match telemetry.membership.get().and_then(|read| read().ok()) {
            Some(state) => state.write(&mut output).map_err(|_| Error::Internal)?,
            None => output
                .write_str("unavailable fully_applied=0\n")
                .map_err(|_| Error::Internal)?,
        }
    } else if route == Route::SendCrc {
        telemetry
            .send_crc
            .write(&mut output)
            .map_err(|_| Error::Internal)?;
    } else if route == Route::Aead {
        telemetry
            .failures
            .write_aead(&mut output)
            .map_err(|_| Error::Internal)?;
    } else if route == Route::Failures {
        telemetry
            .failures
            .write(&mut output)
            .map_err(|_| Error::Internal)?;
    } else {
        output.write_str(body).map_err(|_| Error::Internal)?;
    }
    let body_len = output.used;
    let mut output = Output {
        bytes: header,
        used: 0,
    };
    let content_type = if route == Route::Metrics {
        "text/plain; version=0.0.4; charset=utf-8"
    } else {
        "text/plain; charset=utf-8"
    };
    write!(output, "HTTP/1.1 {status}\r\nContent-Type: {content_type}\r\nContent-Length: {body_len}\r\nConnection: close\r\nCache-Control: no-store\r\n").map_err(|_| Error::Internal)?;
    if route == Route::Method {
        output
            .write_str("Allow: GET\r\n")
            .map_err(|_| Error::Internal)?;
    }
    output.write_str("\r\n").map_err(|_| Error::Internal)?;
    let header_len = output.used;
    bytes.copy_within(HEAD..HEAD + body_len, header_len);
    Ok(header_len + body_len)
}

#[cfg(test)]
mod tests;
