//! Bounded-cardinality diagnostics, independent of data-path admission.
#[path = "telemetry/health.rs"]
pub mod health;
#[path = "telemetry/metrics.rs"]
pub mod metrics;
#[path = "telemetry/server.rs"]
pub mod server;
#[path = "telemetry/tracing.rs"]
pub mod tracing;

use crate::{
    error::{Error, Operation, Result},
    runtime::{admission::Admission, deadline::RequestScope, reactor::Reactor},
};
use std::{cell::OnceCell, net::SocketAddr, rc::Rc};

#[derive(Default)]
pub struct Telemetry {
    pub metrics: metrics::Metrics,
    pub health: health::Health,
    pub tracing: tracing::Tracing,
    io: OnceCell<Rc<server::DiagnosticIo>>,
}
impl Telemetry {
    /// Call on the chosen I/O worker before data admission. Reserves bounded
    /// diagnostic memory/control slots and initializes the supplied worker reactor.
    /// Does not bind a listener or spawn any work. Duplicate attachment is rejected.
    pub fn attach_io(&self, reactor: Rc<Reactor>, admission: Rc<Admission>) -> Result<()> {
        if self.io.get().is_some() {
            return Err(Error::InvalidConfiguration);
        }
        let io = Rc::new(server::DiagnosticIo::attach(reactor, admission)?);
        self.io.set(io).map_err(|_| Error::InvalidConfiguration)
    }

    /// Serve node diagnostics with reserved control-progress capacity.
    /// The integrator must poll this future and the worker reactor. Bind errors
    /// occur on the first poll. Cancellation terminates serving; drain the reactor
    /// before dropping the attached resources at worker shutdown.
    pub fn serve<'a>(&'a self, address: SocketAddr, scope: &'a RequestScope) -> Operation<'a, ()> {
        Box::pin(async move {
            let io = self.io.get().ok_or(Error::InvalidConfiguration)?.clone();
            self.serve_with_io(address, io, scope).await
        })
    }
    /// Explicit alternative for integrators that retain attachment separately.
    pub fn serve_with_io<'a>(
        &'a self,
        address: SocketAddr,
        io: Rc<server::DiagnosticIo>,
        scope: &'a RequestScope,
    ) -> Operation<'a, ()> {
        Box::pin(async move {
            scope.check()?;
            let listener = std::net::TcpListener::bind(address).map_err(|_| Error::Io)?;
            self.serve_listener_with_io(listener, io, scope).await
        })
    }
    /// Ownership-transfer hook also permits port-zero binding and socket tests.
    pub fn serve_listener_with_io<'a>(
        &'a self,
        listener: std::net::TcpListener,
        io: Rc<server::DiagnosticIo>,
        scope: &'a RequestScope,
    ) -> Operation<'a, ()> {
        server::serve(self, listener, io, scope)
    }
}
