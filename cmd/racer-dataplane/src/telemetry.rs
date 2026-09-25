//! Bounded-cardinality diagnostics, independent of data-path admission.
pub mod health;
pub mod metrics;
pub mod tracing;

pub struct Telemetry {
    pub metrics: metrics::Metrics,
    pub health: health::Health,
    pub tracing: tracing::Tracing,
}
impl Default for Telemetry {
    fn default() -> Self {
        Self {
            metrics: metrics::Metrics,
            health: health::Health,
            tracing: tracing::Tracing,
        }
    }
}
impl Telemetry {
    /// Serve node diagnostics with reserved control-progress capacity.
    pub fn serve<'a>(
        &'a self,
        _address: std::net::SocketAddr,
        _scope: &'a crate::runtime::deadline::RequestScope,
    ) -> crate::error::Operation<'a, ()> {
        crate::error::deferred("telemetry.serve")
    }
}
