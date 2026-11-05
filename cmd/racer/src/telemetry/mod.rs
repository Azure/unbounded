use crate::admission::AdmissionPolicy;
use crate::admission::ResourceClass;
use crate::error::Error;
use crate::error::Operation;
use crate::error::Result;
use crate::model::AttemptId;
use crate::model::RequestId;
use crate::model::WorkerId;
use crate::runtime::Reactor;
use crate::runtime::RequestScope;
use crate::telemetry::primitives::Ring;
use crate::telemetry::primitives::SampleLimits;
use crate::telemetry::primitives::SharedRing;

#[cfg(test)]
use crate::rt::environment;
use crate::rt::environment::Deadline;
use crate::telemetry::primitives::server;
use crate::telemetry::primitives::server::Handler;
use crate::telemetry::primitives::server::Response;
use crate::telemetry::primitives::server::Server;
use racer_control_wire::NodeId;
use racer_io::ring::Descriptor;
use std::cell::OnceCell;
use std::fmt::Write;
use std::net::SocketAddr;
#[cfg(test)]
use std::net::TcpListener;
use std::rc::Rc;
use std::sync::Arc;
use std::sync::Mutex;
use std::sync::OnceLock;
#[cfg(test)]
use std::task::Poll;
use std::time::Duration;
use std::time::Instant;

mod diagnostics;
mod forensics;
mod health;
mod metrics;
mod samples;
pub mod primitives;

pub use diagnostics::*;
pub use forensics::*;
pub use health::*;
pub use metrics::*;
pub use samples::*;

pub use server::CONNECTION_TIMEOUT;
pub use server::MAX_CONNECTIONS;
pub use server::MAX_REQUEST_BYTES;
pub use server::MAX_RESPONSE_BYTES;

/// Prepaid diagnostic submissions: the transport's connection slots plus the
/// waits for one in-flight `/debug/hardware` producer, which is always attached.
pub const CONTROL_SLOTS: usize = server::CONTROL_SLOTS + server::JSON_CONTROL_SLOTS;

/// Prepaid diagnostic memory: transport buffers, the hardware producer's wait
/// bookkeeping, and the `/debug/hardware` helper's peak footprint.
///
/// At most one helper exists at a time. Its peak is during encoding, when the
/// discovered report (charged against `hardware::WORKING_BYTES` by
/// `hardware::Walk`, which aborts with `response_too_large` when exhausted) is
/// alive alongside the JSON buffer. The JSON buffer is allocated once with
/// capacity `HARDWARE_BODY_BYTES` (see `Bounded`), never grows, and becomes the
/// result held for the response, so one `HARDWARE_BODY_BYTES` term covers both
/// the encoding buffer and the held result. The report is dropped before the
/// result is published. The detached discovery helper's thread stack
/// (`HARDWARE_STACK_BYTES`) is charged too, since a blocked sysfs read can keep
/// the helper alive after diagnostics teardown.
pub const RESERVED_BYTES: usize = server::RESERVED_BYTES
    + server::JSON_CONTROL_SLOTS * racer_io::scoped::SUBMISSION_BYTES
    + HARDWARE_BODY_BYTES
    + crate::hardware::WORKING_BYTES
    + HARDWARE_STACK_BYTES;

/// Stack size of the `/debug/hardware` discovery helper thread.
const HARDWARE_STACK_BYTES: usize = 2 * 1024 * 1024;

const _: () = assert!(server::MAX_BODY_BYTES <= server::MAX_RESPONSE_BYTES);
const _: () = assert!(HARDWARE_BODY_BYTES <= server::MAX_BODY_BYTES);

/// Largest `/debug/hardware` JSON body. Kept at its original 64 KiB bound,
/// independent of the larger response buffer `/metrics` needs, so the
/// hardware working budget does not grow with it.
pub(crate) const HARDWARE_BODY_BYTES: usize = 64 * 1024 - 256;
const _: () = assert!(RESERVED_BYTES >= server::RESERVED_BYTES + HARDWARE_BYTES);

/// Deadline for one `/debug/hardware` request, covering discovery and the response.
pub(crate) const HARDWARE_DEADLINE: Duration = Duration::from_secs(5);

/// Owned, `Send` hardware discovery prepared on the control worker and run on a
/// helper thread, so blocking sysfs reads never stall the worker. The job charges
/// the walk's working budget and stops early once the request is cancelled.
pub(crate) type HardwareJob = Box<
    dyn FnOnce(
            &crate::hardware::Walk,
        ) -> std::result::Result<crate::hardware::Report, crate::hardware::Abort>
        + Send,
>;

// Bounded diagnostics and HTTP endpoints, polled by an existing worker.

#[derive(Default)]
pub struct Telemetry {
    pub(crate) profiling: Option<Arc<crate::profiling::ProfilingController>>,
    pub(crate) membership: OnceCell<Rc<dyn Fn() -> Result<MembershipDiagnostic>>>,
    /// Prepares live hardware discovery for `/debug/hardware`; installed on the
    /// control worker only. The returned job runs on a helper thread.
    pub(crate) hardware: OnceCell<Rc<dyn Fn() -> HardwareJob>>,
    /// The current hardware helper, alive until both its thread and response finish.
    hardware_active: std::cell::RefCell<std::sync::Weak<HardwareState>>,
    pub send_tag: crate::telemetry::Samples,
    pub failures: crate::telemetry::Failures,
    pub metrics: crate::telemetry::Metrics,
    pub health: crate::telemetry::Health,
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
        let io = Rc::new(DiagnosticIo::attach_configured(
            reactor,
            admission,
            self.profiling.is_some(),
        )?);
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
            let listener =
                racer_io::scoped::descriptor::tcp_listener(io.reactor.sim_os().as_ref(), address)?;
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
