//! Fixed series only. Callers cannot supply metric names or label values.
use crate::error::Result;
use std::sync::{
    Arc,
    atomic::{AtomicU64, Ordering},
};

pub const EVENT_COUNT: usize = 14;
pub const GAUGE_COUNT: usize = 3;
#[derive(Clone, Default)]
pub struct Metrics(Arc<Counters>);
#[derive(Default)]
struct Counters {
    events: [AtomicU64; EVENT_COUNT],
    gauges: [AtomicU64; GAUGE_COUNT],
}
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
#[repr(usize)]
pub enum Event {
    MemoryHit,
    DiskHit,
    PeerHit,
    OriginFill,
    DirtyDiscard,
    Overload,
    CorruptMiss,
    DiagnosticAccepted,
    DiagnosticHealth,
    DiagnosticReady,
    DiagnosticMetrics,
    DiagnosticRejected,
    DiagnosticIoError,
    DiagnosticTimeout,
}
pub const EVENTS: [Event; EVENT_COUNT] = [
    Event::MemoryHit,
    Event::DiskHit,
    Event::PeerHit,
    Event::OriginFill,
    Event::DirtyDiscard,
    Event::Overload,
    Event::CorruptMiss,
    Event::DiagnosticAccepted,
    Event::DiagnosticHealth,
    Event::DiagnosticReady,
    Event::DiagnosticMetrics,
    Event::DiagnosticRejected,
    Event::DiagnosticIoError,
    Event::DiagnosticTimeout,
];
impl Event {
    pub fn name(self) -> &'static str {
        match self {
            Self::MemoryHit => "racer_memory_hits_total",
            Self::DiskHit => "racer_disk_hits_total",
            Self::PeerHit => "racer_peer_hits_total",
            Self::OriginFill => "racer_origin_fills_total",
            Self::DirtyDiscard => "racer_dirty_discards_total",
            Self::Overload => "racer_overloads_total",
            Self::CorruptMiss => "racer_corrupt_misses_total",
            Self::DiagnosticAccepted => "racer_diagnostic_accepted_total",
            Self::DiagnosticHealth => "racer_diagnostic_health_total",
            Self::DiagnosticReady => "racer_diagnostic_ready_total",
            Self::DiagnosticMetrics => "racer_diagnostic_metrics_total",
            Self::DiagnosticRejected => "racer_diagnostic_rejected_total",
            Self::DiagnosticIoError => "racer_diagnostic_io_errors_total",
            Self::DiagnosticTimeout => "racer_diagnostic_timeouts_total",
        }
    }
}
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
#[repr(usize)]
pub enum Gauge {
    DiagnosticConnections,
    ActiveRequests,
    ActiveFills,
}
pub const GAUGES: [Gauge; GAUGE_COUNT] = [
    Gauge::DiagnosticConnections,
    Gauge::ActiveRequests,
    Gauge::ActiveFills,
];
impl Gauge {
    pub fn name(self) -> &'static str {
        match self {
            Self::DiagnosticConnections => "racer_diagnostic_connections",
            Self::ActiveRequests => "racer_active_requests",
            Self::ActiveFills => "racer_active_fills",
        }
    }
}
/// Keep with the actual resource, including through a submitted I/O fence.
pub struct GaugeLease {
    metrics: Metrics,
    gauge: Gauge,
}
impl Drop for GaugeLease {
    fn drop(&mut self) {
        self.metrics.0.gauges[self.gauge as usize].fetch_sub(1, Ordering::Relaxed);
    }
}
impl Metrics {
    /// Saturate instead of wrapping a long-lived Prometheus counter.
    pub fn record(&self, event: Event, amount: u64) -> Result<()> {
        let _ = self.0.events[event as usize].fetch_update(
            Ordering::Relaxed,
            Ordering::Relaxed,
            |old| Some(old.saturating_add(amount)),
        );
        Ok(())
    }
    pub fn count(&self, event: Event) -> u64 {
        self.0.events[event as usize].load(Ordering::Relaxed)
    }
    pub fn gauge(&self, gauge: Gauge) -> u64 {
        self.0.gauges[gauge as usize].load(Ordering::Relaxed)
    }
    pub fn lease(&self, gauge: Gauge) -> Result<GaugeLease> {
        self.0.gauges[gauge as usize]
            .fetch_update(Ordering::Relaxed, Ordering::Relaxed, |old| {
                old.checked_add(1)
            })
            .map_err(|_| crate::error::Error::Overloaded)?;
        Ok(GaugeLease {
            metrics: self.clone(),
            gauge,
        })
    }
    /// The destination is bounded by the diagnostic server; no intermediate String.
    pub fn write_prometheus(&self, out: &mut impl std::fmt::Write) -> std::fmt::Result {
        for event in EVENTS {
            writeln!(
                out,
                "# TYPE {} counter\n{} {}",
                event.name(),
                event.name(),
                self.count(event)
            )?;
        }
        for gauge in GAUGES {
            writeln!(
                out,
                "# TYPE {} gauge\n{} {}",
                gauge.name(),
                gauge.name(),
                self.gauge(gauge)
            )?;
        }
        Ok(())
    }
}
#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn fixed_series_saturate_and_leases_return_to_baseline() {
        let metrics = Metrics::default();
        for event in EVENTS {
            metrics.record(event, u64::MAX).unwrap();
            metrics.record(event, 9).unwrap();
            assert_eq!(metrics.count(event), u64::MAX);
        }
        let lease = metrics.lease(Gauge::ActiveRequests).unwrap();
        assert_eq!(metrics.clone().gauge(Gauge::ActiveRequests), 1);
        drop(lease);
        assert_eq!(metrics.gauge(Gauge::ActiveRequests), 0);
        let mut output = String::new();
        metrics.write_prometheus(&mut output).unwrap();
        assert_eq!(
            output.lines().filter(|line| !line.starts_with('#')).count(),
            EVENT_COUNT + GAUGE_COUNT
        );
        assert!(!output.contains('{'));
        assert!(output.len() < 4096);
    }
}
