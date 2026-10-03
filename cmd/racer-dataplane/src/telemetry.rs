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
            self.state_at(uring_runtime::environment::now())
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
                        .usable_at(uring_runtime::environment::now())
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
            let now = uring_runtime::environment::now();
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
        telemetry::failures::AeadFailure,
    };
    use ::telemetry::Ring;
    use std::{
        sync::{Arc, Mutex},
        time::{Duration, Instant},
    };
    use uring_runtime::environment;
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
                || !racer_identity::canonical_uuid(&self.sender.0)
                || !racer_identity::canonical_uuid(&self.receiver.0)
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
        entries: Ring<Arc<Sample>, CAPACITY>,
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
                entries: Ring::default(),
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
            state.entries.push(sample.clone());
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
            for (_, sample) in entries.iter_refs() {
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
        fn send_crc_interval_accepts_exactly_500ms() {
            let clock = environment::SimulationClock::new(73);
            let _env = clock.environment(0).enter();
            let _strict = environment::require_simulated();
            let p = pair();
            let samples = Samples::default();
            let (ticket, work) = samples.begin(&p, &p.sender, &p.receiver).unwrap();
            drop(work);
            drop(ticket);
            clock.advance(Duration::from_millis(500) - Duration::from_nanos(1));
            assert!(samples.begin(&p, &p.sender, &p.receiver).is_none());
            clock.advance(Duration::from_nanos(1));
            let (ticket, work) = samples.begin(&p, &p.sender, &p.receiver).unwrap();
            assert_eq!(ticket.0.sequence, 2);
            drop(work);
            drop(ticket);
            let state = samples.0.lock().unwrap();
            assert_eq!((state.eligible, state.sampled, state.skipped), (3, 2, 1));
            assert!(!state.busy);
        }

        #[test]
        fn send_crc_ticket_and_snapshot_survive_retention_overwrite() {
            let clock = environment::SimulationClock::new(74);
            let _env = clock.environment(0).enter();
            let _strict = environment::require_simulated();
            let p = pair();
            let samples = Samples::default();
            let (ticket, work) = samples.begin(&p, &p.sender, &p.receiver).unwrap();
            let weak = Arc::downgrade(&ticket.0);
            work.finish(None);
            drop(work);
            let snapshot = samples.0.lock().unwrap().entries.clone();
            for _ in 0..CAPACITY {
                clock.advance(Duration::from_millis(500));
                let (next, work) = samples.begin(&p, &p.sender, &p.receiver).unwrap();
                next.finish(true);
                work.finish(None);
            }
            let mut before = String::new();
            samples.write(&mut before).unwrap();
            assert!(before.starts_with(
                "eligible=65 sampled=65 skipped=0 busy=0 retained=64 overwritten=1 capacity=64\nseq=2 "
            ));
            assert_eq!(before.lines().count(), CAPACITY + 1);
            ticket.finish(false);
            let mut after = String::new();
            samples.write(&mut after).unwrap();
            assert_eq!(
                before, after,
                "evicted Ticket cannot change retained records"
            );
            let (_, retained) = snapshot.iter_refs().next().unwrap();
            assert!(Arc::ptr_eq(retained, &ticket.0));
            assert_eq!(retained.data.lock().unwrap().send, "failed");
            drop(ticket);
            assert!(
                weak.upgrade().is_some(),
                "snapshot retains the evicted sample"
            );
            drop(snapshot);
            assert!(weak.upgrade().is_none(), "last handle releases the sample");
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

use crate::{
    error::{Error, Operation, Result},
    model::ResourceClass,
    runtime::{
        admission::AdmissionPolicy,
        deadline::{Deadline, RequestScope},
        reactor::Reactor,
    },
};
use ::telemetry::server::{self, Handler, Response, Server};
use metrics::{Event, Gauge};
#[cfg(test)]
use std::net::TcpListener;
#[cfg(test)]
use std::time::Instant;
use std::{cell::OnceCell, fmt::Write, net::SocketAddr, rc::Rc};
#[cfg(test)]
use std::{task::Poll, time::Duration};
use uring_runtime::reactor::Descriptor;

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
    pub fn attach_io(
        &self,
        reactor: Rc<Reactor>,
        admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
    ) -> Result<()> {
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

pub use server::{
    CONNECTION_TIMEOUT, CONTROL_SLOTS, MAX_CONNECTIONS, MAX_REQUEST_BYTES, MAX_RESPONSE_BYTES,
    RESERVED_BYTES,
};

impl server::Scope for RequestScope {
    fn with_deadline(&self, deadline: std::time::Instant) -> Self {
        Self {
            deadline: Deadline(self.deadline.0.min(deadline)),
            ..self.clone()
        }
    }
}

/// One attachment serves at most one listener. Resource reservations outlive
/// abandoned connection futures because the reactor retains their buffers.
pub struct DiagnosticIo {
    reactor: Rc<Reactor>,
    admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
    server: Server,
}
impl DiagnosticIo {
    /// Explicit startup acquisition, using the worker's already-budgeted reactor.
    /// Must run before ordinary request admission fills the memory quota.
    pub fn attach(
        reactor: Rc<Reactor>,
        admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
    ) -> Result<Self> {
        let control = admission.reserve(None, ResourceClass::ControlProgress, CONTROL_SLOTS)?;
        let mut memory = admission.reserve(None, ResourceClass::RequestContext, RESERVED_BYTES)?;
        let bookkeeping = memory.split(CONTROL_SLOTS * uring_runtime::reactor::SUBMISSION_BYTES)?;
        let submissions = reactor.reserve_submissions(control, bookkeeping)?;
        Ok(Self {
            reactor,
            admission,
            server: Server::new(submissions, memory),
        })
    }
}

fn serve<'a>(
    telemetry: &'a Telemetry,
    listener: Descriptor,
    io: Rc<DiagnosticIo>,
    scope: &'a RequestScope,
) -> Operation<'a, ()> {
    Box::pin(async move {
        let handler = DiagnosticHandler {
            telemetry,
            admission: &io.admission,
        };
        io.server
            .serve(&io.reactor, listener, &handler, scope)
            .await
    })
}

struct DiagnosticHandler<'a> {
    telemetry: &'a Telemetry,
    admission: &'a flow_control::Quotas<AdmissionPolicy>,
}
impl Handler for DiagnosticHandler<'_> {
    type Connection = ::telemetry::Lease;
    fn connect(&self) -> Option<::telemetry::Lease> {
        self.telemetry
            .metrics
            .lease(Gauge::DiagnosticConnections)
            .ok()
    }
    fn get(
        &self,
        path: &str,
        out: &mut dyn Write,
    ) -> std::result::Result<Response, std::fmt::Error> {
        get(self.telemetry, path, !self.admission.is_stopped(), out)
    }
    fn observe(&self, event: server::Event) {
        let event = match event {
            server::Event::Accepted => Event::DiagnosticAccepted,
            server::Event::Rejected => Event::DiagnosticRejected,
            server::Event::IoError => Event::DiagnosticIoError,
            server::Event::Timeout => Event::DiagnosticTimeout,
        };
        let _ = self.telemetry.metrics.record(event, 1);
    }
}
fn get(
    telemetry: &Telemetry,
    path: &str,
    admission_usable: bool,
    mut output: &mut dyn Write,
) -> std::result::Result<Response, std::fmt::Error> {
    let ready = admission_usable && telemetry.health.ready();
    let (response, body, event) = match path {
        "/healthz" if telemetry.health.live() => (Response::Text, "ok\n", Event::DiagnosticHealth),
        "/healthz" => (Response::Unavailable, "not live\n", Event::DiagnosticHealth),
        "/readyz" if ready => (Response::Text, "ready\n", Event::DiagnosticReady),
        "/readyz" => (Response::Unavailable, "not ready\n", Event::DiagnosticReady),
        "/metrics" => (Response::Metrics, "", Event::DiagnosticMetrics),
        "/debug/membership" => (Response::Text, "", Event::DiagnosticMetrics),
        "/debug/failures" | "/debug/aead" | "/debug/send-crc" => {
            (Response::Text, "", Event::DiagnosticFailures)
        }
        _ => return Ok(Response::NotFound),
    };
    telemetry
        .metrics
        .record(event, 1)
        .map_err(|_| std::fmt::Error)?;
    if path == "/metrics" {
        telemetry.metrics.write_prometheus(&mut output)?;
        writeln!(
            output,
            "# TYPE racer_ready gauge\nracer_ready {}\n# TYPE racer_live gauge\nracer_live {}",
            u8::from(ready),
            u8::from(telemetry.health.live())
        )?;
    } else if path == "/debug/membership" {
        match telemetry.membership.get().and_then(|read| read().ok()) {
            Some(state) => state.write(&mut output)?,
            None => output.write_str("unavailable fully_applied=0\n")?,
        }
    } else if path == "/debug/send-crc" {
        telemetry.send_crc.write(&mut output)?;
    } else if path == "/debug/aead" {
        telemetry.failures.write_aead(&mut output)?;
    } else if path == "/debug/failures" {
        telemetry.failures.write(&mut output)?;
    } else {
        output.write_str(body)?;
    }
    Ok(response)
}

#[cfg(test)]
mod tests;
