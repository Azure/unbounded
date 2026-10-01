//! Fixed series only. Callers cannot supply metric names or label values.
use crate::error::Result;
use std::sync::{
    Arc,
    atomic::{AtomicU64, Ordering},
};

pub const EVENT_COUNT: usize = 61;
pub const GAUGE_COUNT: usize = 13;
#[derive(Clone, Copy)]
pub(crate) enum LookupTier {
    Plaintext,
    Ciphertext,
    Pending,
    DiskIndex,
}
/// Clones retain their writer shard; reads aggregate the fixed node registry.
#[derive(Clone)]
pub struct Metrics {
    registry: Arc<Registry>,
    shard: usize,
}
struct Registry {
    // Retain every shard until the registry is dropped, even after a worker exits.
    shards: Box<[Counters]>,
    gauges: [GaugeCounter; GAUGE_COUNT],
}
// Match the runtime's 64-byte cache-line policy. Padding the entire writer block
// avoids false sharing between workers without padding every event separately.
#[repr(align(64))]
struct Counters {
    events: [AtomicU64; EVENT_COUNT],
}
impl Default for Counters {
    fn default() -> Self {
        Self {
            events: std::array::from_fn(|_| AtomicU64::new(0)),
        }
    }
}
// Gauges retain exact node-wide lease overflow checks and replacement semantics.
// Separate lines prevent unrelated resource classes from invalidating each other.
#[repr(align(64))]
#[derive(Default)]
struct GaugeCounter(AtomicU64);

impl Default for Metrics {
    fn default() -> Self {
        Self::for_workers(1)
            .expect("one metrics worker")
            .pop()
            .unwrap()
    }
}
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
#[repr(usize)]
pub enum Event {
    PageHedgeStarted,
    PageHedgeWon,
    PageHedgeSuppressed,
    PageHedgeDuplicateBytes,
    PeerAdmissionAccepted,
    PeerAdmissionRejected,
    PeerCircuitRejected,
    PeerProbe,
    PeerVerified,
    PeerLinkFailure,
    PeerLocalPressure,
    Request,
    RequestError,
    MemoryHit,
    DiskHit,
    PeerHit,
    OriginFill,
    DirtyDiscard,
    DiskPublication,
    Overload,
    CorruptMiss,
    DiagnosticAccepted,
    DiagnosticHealth,
    DiagnosticReady,
    DiagnosticMetrics,
    DiagnosticRejected,
    DiagnosticIoError,
    DiagnosticTimeout,
    PageDecrypt,
    PeerBootstrap,
    DiagnosticFailures,
    DeliveryPipeDrain,
    DeliveryDirectBytes,
    CryptoEncryptStarted,
    CryptoEncryptSuccess,
    CryptoEncryptFailure,
    CryptoEncryptBytes,
    CryptoEncryptExecutionCount,
    CryptoEncryptExecutionNs,
    CryptoEncryptQueueCount,
    CryptoEncryptQueueNs,
    CryptoDecryptStarted,
    CryptoDecryptSuccess,
    CryptoDecryptFailure,
    CryptoDecryptBytes,
    CryptoDecryptExecutionCount,
    CryptoDecryptExecutionNs,
    CryptoDecryptQueueCount,
    CryptoDecryptQueueNs,
    PlaintextLookupHit,
    PlaintextLookupMiss,
    PlaintextLookupError,
    CiphertextLookupHit,
    CiphertextLookupMiss,
    CiphertextLookupError,
    PendingLookupHit,
    PendingLookupMiss,
    PendingLookupError,
    DiskIndexLookupHit,
    DiskIndexLookupMiss,
    DiskIndexLookupError,
}
pub const EVENTS: [Event; EVENT_COUNT] = [
    Event::PageHedgeStarted,
    Event::PageHedgeWon,
    Event::PageHedgeSuppressed,
    Event::PageHedgeDuplicateBytes,
    Event::PeerAdmissionAccepted,
    Event::PeerAdmissionRejected,
    Event::PeerCircuitRejected,
    Event::PeerProbe,
    Event::PeerVerified,
    Event::PeerLinkFailure,
    Event::PeerLocalPressure,
    Event::Request,
    Event::RequestError,
    Event::MemoryHit,
    Event::DiskHit,
    Event::PeerHit,
    Event::OriginFill,
    Event::DirtyDiscard,
    Event::DiskPublication,
    Event::Overload,
    Event::CorruptMiss,
    Event::DiagnosticAccepted,
    Event::DiagnosticHealth,
    Event::DiagnosticReady,
    Event::DiagnosticMetrics,
    Event::DiagnosticRejected,
    Event::DiagnosticIoError,
    Event::DiagnosticTimeout,
    Event::PageDecrypt,
    Event::PeerBootstrap,
    Event::DiagnosticFailures,
    Event::DeliveryPipeDrain,
    Event::DeliveryDirectBytes,
    Event::CryptoEncryptStarted,
    Event::CryptoEncryptSuccess,
    Event::CryptoEncryptFailure,
    Event::CryptoEncryptBytes,
    Event::CryptoEncryptExecutionCount,
    Event::CryptoEncryptExecutionNs,
    Event::CryptoEncryptQueueCount,
    Event::CryptoEncryptQueueNs,
    Event::CryptoDecryptStarted,
    Event::CryptoDecryptSuccess,
    Event::CryptoDecryptFailure,
    Event::CryptoDecryptBytes,
    Event::CryptoDecryptExecutionCount,
    Event::CryptoDecryptExecutionNs,
    Event::CryptoDecryptQueueCount,
    Event::CryptoDecryptQueueNs,
    Event::PlaintextLookupHit,
    Event::PlaintextLookupMiss,
    Event::PlaintextLookupError,
    Event::CiphertextLookupHit,
    Event::CiphertextLookupMiss,
    Event::CiphertextLookupError,
    Event::PendingLookupHit,
    Event::PendingLookupMiss,
    Event::PendingLookupError,
    Event::DiskIndexLookupHit,
    Event::DiskIndexLookupMiss,
    Event::DiskIndexLookupError,
];
impl Event {
    pub fn name(self) -> &'static str {
        match self {
            Self::PageHedgeStarted => "racer_page_hedges_started_total",
            Self::PageHedgeWon => "racer_page_hedges_won_total",
            Self::PageHedgeSuppressed => "racer_page_hedges_suppressed_total",
            Self::PageHedgeDuplicateBytes => "racer_page_hedge_duplicate_reserved_bytes_total",
            Self::PeerAdmissionAccepted => "racer_peer_admission_accepted_total",
            Self::PeerAdmissionRejected => "racer_peer_admission_rejected_total",
            Self::PeerCircuitRejected => "racer_peer_circuit_rejected_total",
            Self::PeerProbe => "racer_peer_probes_total",
            Self::PeerVerified => "racer_peer_verified_responses_total",
            Self::PeerLinkFailure => "racer_peer_link_failures_total",
            Self::PeerLocalPressure => "racer_peer_local_pressure_total",
            Self::Request => "racer_requests_total",
            Self::RequestError => "racer_request_errors_total",
            Self::MemoryHit => "racer_memory_hits_total",
            Self::DiskHit => "racer_disk_hits_total",
            Self::PeerHit => "racer_peer_hits_total",
            Self::OriginFill => "racer_origin_fills_total",
            Self::DirtyDiscard => "racer_dirty_discards_total",
            Self::DiskPublication => "racer_disk_publications_total",
            Self::Overload => "racer_overloads_total",
            Self::CorruptMiss => "racer_corrupt_misses_total",
            Self::DiagnosticAccepted => "racer_diagnostic_accepted_total",
            Self::DiagnosticHealth => "racer_diagnostic_health_total",
            Self::DiagnosticReady => "racer_diagnostic_ready_total",
            Self::DiagnosticMetrics => "racer_diagnostic_metrics_total",
            Self::DiagnosticRejected => "racer_diagnostic_rejected_total",
            Self::DiagnosticIoError => "racer_diagnostic_io_errors_total",
            Self::DiagnosticTimeout => "racer_diagnostic_timeouts_total",
            Self::PageDecrypt => "racer_page_decrypts_total",
            Self::PeerBootstrap => "racer_peer_bootstraps_total",
            Self::DiagnosticFailures => "racer_diagnostic_failures_total",
            Self::DeliveryPipeDrain => "racer_delivery_pipe_drains_total",
            Self::DeliveryDirectBytes => "racer_delivery_direct_bytes_total",
            Self::CryptoEncryptStarted => "racer_crypto_encrypt_started_total",
            Self::CryptoEncryptSuccess => "racer_crypto_encrypt_success_total",
            Self::CryptoEncryptFailure => "racer_crypto_encrypt_failure_total",
            Self::CryptoEncryptBytes => "racer_crypto_encrypt_success_bytes_total",
            Self::CryptoEncryptExecutionCount => "racer_crypto_encrypt_execution_nanoseconds_count",
            Self::CryptoEncryptExecutionNs => "racer_crypto_encrypt_execution_nanoseconds_sum",
            Self::CryptoEncryptQueueCount => "racer_crypto_encrypt_queue_nanoseconds_count",
            Self::CryptoEncryptQueueNs => "racer_crypto_encrypt_queue_nanoseconds_sum",
            Self::CryptoDecryptStarted => "racer_crypto_decrypt_started_total",
            Self::CryptoDecryptSuccess => "racer_crypto_decrypt_success_total",
            Self::CryptoDecryptFailure => "racer_crypto_decrypt_failure_total",
            Self::CryptoDecryptBytes => "racer_crypto_decrypt_success_bytes_total",
            Self::CryptoDecryptExecutionCount => "racer_crypto_decrypt_execution_nanoseconds_count",
            Self::CryptoDecryptExecutionNs => "racer_crypto_decrypt_execution_nanoseconds_sum",
            Self::CryptoDecryptQueueCount => "racer_crypto_decrypt_queue_nanoseconds_count",
            Self::CryptoDecryptQueueNs => "racer_crypto_decrypt_queue_nanoseconds_sum",
            Self::PlaintextLookupHit => "racer_plaintext_lookup_hits_total",
            Self::PlaintextLookupMiss => "racer_plaintext_lookup_misses_total",
            Self::PlaintextLookupError => "racer_plaintext_lookup_errors_total",
            Self::CiphertextLookupHit => "racer_ciphertext_lookup_hits_total",
            Self::CiphertextLookupMiss => "racer_ciphertext_lookup_misses_total",
            Self::CiphertextLookupError => "racer_ciphertext_lookup_errors_total",
            Self::PendingLookupHit => "racer_pending_lookup_hits_total",
            Self::PendingLookupMiss => "racer_pending_lookup_misses_total",
            Self::PendingLookupError => "racer_pending_lookup_errors_total",
            Self::DiskIndexLookupHit => "racer_disk_index_lookup_hits_total",
            Self::DiskIndexLookupMiss => "racer_disk_index_lookup_misses_total",
            Self::DiskIndexLookupError => "racer_disk_index_lookup_errors_total",
        }
    }
}
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
#[repr(usize)]
pub enum Gauge {
    PeerAdmissionLimit,
    PeerExchanges,
    DiagnosticConnections,
    ActiveRequests,
    ActiveFills,
    KeyringGeneration,
    IdentityExpiresAtSeconds,
    PendingDiskWrites,
    ActiveDeliveries,
    EffectivePayloadBytes,
    SegmentTailBytes,
    DiskPageEntries,
    CheckpointSequence,
}
pub const GAUGES: [Gauge; GAUGE_COUNT] = [
    Gauge::PeerAdmissionLimit,
    Gauge::PeerExchanges,
    Gauge::DiagnosticConnections,
    Gauge::ActiveRequests,
    Gauge::ActiveFills,
    Gauge::KeyringGeneration,
    Gauge::IdentityExpiresAtSeconds,
    Gauge::PendingDiskWrites,
    Gauge::ActiveDeliveries,
    Gauge::EffectivePayloadBytes,
    Gauge::SegmentTailBytes,
    Gauge::DiskPageEntries,
    Gauge::CheckpointSequence,
];
impl Gauge {
    pub fn name(self) -> &'static str {
        match self {
            Self::PeerAdmissionLimit => "racer_peer_admission_limit",
            Self::PeerExchanges => "racer_peer_exchanges_active",
            Self::DiagnosticConnections => "racer_diagnostic_connections",
            Self::ActiveRequests => "racer_active_requests",
            Self::ActiveFills => "racer_active_fills",
            Self::KeyringGeneration => "racer_keyring_generation",
            Self::IdentityExpiresAtSeconds => "racer_identity_expires_at_seconds",
            Self::PendingDiskWrites => "racer_pending_disk_writes",
            Self::ActiveDeliveries => "racer_active_deliveries",
            Self::EffectivePayloadBytes => "racer_effective_payload_bytes",
            Self::SegmentTailBytes => "racer_segment_tail_bytes",
            Self::DiskPageEntries => "racer_disk_page_index_capacity",
            Self::CheckpointSequence => "racer_checkpoint_sequence",
        }
    }
}
/// Keep with the actual resource, including through a submitted I/O fence.
pub struct GaugeLease {
    metrics: Metrics,
    gauge: Gauge,
}
/// One complete client head through final delivery, including cancellation/drop.
pub(crate) struct RequestMetrics {
    active: GaugeLease,
    succeeded: bool,
    overloaded: bool,
}
impl RequestMetrics {
    pub(crate) fn success(&mut self) {
        self.succeeded = true;
    }
    pub(crate) fn fail(&mut self, error: crate::error::Error) {
        if error == crate::error::Error::Overloaded && !self.overloaded {
            let _ = self.active.metrics.record(Event::Overload, 1);
            self.overloaded = true;
        }
    }
}
impl Drop for RequestMetrics {
    fn drop(&mut self) {
        if !self.succeeded {
            let _ = self.active.metrics.record(Event::RequestError, 1);
        }
    }
}
impl Drop for GaugeLease {
    fn drop(&mut self) {
        self.metrics.registry.gauges[self.gauge as usize]
            .0
            .fetch_sub(1, Ordering::Relaxed);
    }
}
impl Metrics {
    /// Observe only an executed synchronous presence probe, preserving its result.
    pub(crate) fn lookup<T>(
        &self,
        tier: LookupTier,
        result: Result<Option<T>>,
    ) -> Result<Option<T>> {
        let events = match tier {
            LookupTier::Plaintext => [
                Event::PlaintextLookupHit,
                Event::PlaintextLookupMiss,
                Event::PlaintextLookupError,
            ],
            LookupTier::Ciphertext => [
                Event::CiphertextLookupHit,
                Event::CiphertextLookupMiss,
                Event::CiphertextLookupError,
            ],
            LookupTier::Pending => [
                Event::PendingLookupHit,
                Event::PendingLookupMiss,
                Event::PendingLookupError,
            ],
            LookupTier::DiskIndex => [
                Event::DiskIndexLookupHit,
                Event::DiskIndexLookupMiss,
                Event::DiskIndexLookupError,
            ],
        };
        let outcome = match &result {
            Ok(Some(_)) => 0,
            Ok(None) => 1,
            Err(_) => 2,
        };
        let _ = self.record(events[outcome], 1);
        result
    }
    /// Allocate a fixed registry at startup, with one event writer per worker.
    /// No registration, locking, or registry reference-count changes on record.
    pub(crate) fn for_workers(count: usize) -> Result<Vec<Self>> {
        if count == 0 {
            return Err(crate::error::Error::InvalidConfiguration);
        }
        let registry = Arc::new(Registry {
            shards: (0..count).map(|_| Counters::default()).collect(),
            gauges: std::array::from_fn(|_| GaugeCounter::default()),
        });
        Ok((0..count)
            .map(|shard| Self {
                registry: registry.clone(),
                shard,
            })
            .collect())
    }

    pub(crate) fn request(&self) -> Result<RequestMetrics> {
        let active = self.lease(Gauge::ActiveRequests)?;
        self.record(Event::Request, 1)?;
        Ok(RequestMetrics {
            active,
            succeeded: false,
            overloaded: false,
        })
    }
    /// Saturate instead of wrapping a long-lived Prometheus counter.
    pub fn record(&self, event: Event, amount: u64) -> Result<()> {
        let _ = self.registry.shards[self.shard].events[event as usize].fetch_update(
            Ordering::Relaxed,
            Ordering::Relaxed,
            |old| Some(old.saturating_add(amount)),
        );
        Ok(())
    }
    pub fn count(&self, event: Event) -> u64 {
        self.registry.shards.iter().fold(0u64, |total, shard| {
            total.saturating_add(shard.events[event as usize].load(Ordering::Relaxed))
        })
    }
    pub fn gauge(&self, gauge: Gauge) -> u64 {
        self.registry.gauges[gauge as usize]
            .0
            .load(Ordering::Relaxed)
    }
    pub(crate) fn set_gauge(&self, gauge: Gauge, value: u64) {
        self.registry.gauges[gauge as usize]
            .0
            .store(value, Ordering::Relaxed);
    }
    pub(crate) fn add_gauge(&self, gauge: Gauge, value: u64) {
        self.registry.gauges[gauge as usize]
            .0
            .fetch_add(value, Ordering::Relaxed);
    }
    pub fn lease(&self, gauge: Gauge) -> Result<GaugeLease> {
        self.registry.gauges[gauge as usize]
            .0
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
    /// Relaxed per-series observations are not a coherent snapshot of all workers.
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
    fn lookup_outcomes_preserve_results_and_export_fixed_series() {
        let metrics = Metrics::default();
        for (tier, hit, miss, error) in [
            (
                LookupTier::Plaintext,
                Event::PlaintextLookupHit,
                Event::PlaintextLookupMiss,
                Event::PlaintextLookupError,
            ),
            (
                LookupTier::Ciphertext,
                Event::CiphertextLookupHit,
                Event::CiphertextLookupMiss,
                Event::CiphertextLookupError,
            ),
            (
                LookupTier::Pending,
                Event::PendingLookupHit,
                Event::PendingLookupMiss,
                Event::PendingLookupError,
            ),
            (
                LookupTier::DiskIndex,
                Event::DiskIndexLookupHit,
                Event::DiskIndexLookupMiss,
                Event::DiskIndexLookupError,
            ),
        ] {
            assert_eq!(metrics.lookup(tier, Ok(Some(7))), Ok(Some(7)));
            assert_eq!(metrics.lookup::<u8>(tier, Ok(None)), Ok(None));
            for failure in [
                crate::error::Error::CorruptRecord,
                crate::error::Error::Io,
                crate::error::Error::MissingKey,
            ] {
                assert_eq!(metrics.lookup::<u8>(tier, Err(failure)), Err(failure));
            }
            assert_eq!(metrics.count(hit), 1);
            assert_eq!(metrics.count(miss), 1);
            assert_eq!(metrics.count(error), 3);
            let mut output = String::new();
            metrics.write_prometheus(&mut output).unwrap();
            assert!(output.contains(&format!(
                "# TYPE {} counter\n{} 1\n",
                hit.name(),
                hit.name()
            )));
            assert!(output.contains(&format!("{} 1\n", miss.name())));
            assert!(output.contains(&format!("{} 3\n", error.name())));
        }
    }
    #[test]
    fn request_drop_counts_failure_once_and_workers_share_counters() {
        let mut workers = Metrics::for_workers(2).unwrap();
        let metrics = workers.pop().unwrap();
        let worker = workers.pop().unwrap();
        std::thread::spawn(move || {
            let mut success = worker.request().unwrap();
            success.success();
            drop(success);
            let mut overloaded = worker.request().unwrap();
            overloaded.fail(crate::error::Error::Overloaded);
            overloaded.fail(crate::error::Error::Overloaded);
            drop(overloaded);
            let abandoned = worker.request().unwrap();
            assert_eq!(worker.gauge(Gauge::ActiveRequests), 1);
            drop(abandoned);
        })
        .join()
        .unwrap();
        assert_eq!(metrics.count(Event::Request), 3);
        assert_eq!(metrics.count(Event::RequestError), 2);
        assert_eq!(metrics.count(Event::Overload), 1);
        assert_eq!(metrics.gauge(Gauge::ActiveRequests), 0);
    }
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
        assert!(output.len() < 8192);
    }

    #[test]
    fn installed_credential_gauges_replace_values_across_shared_handles() {
        let mut workers = Metrics::for_workers(2).unwrap();
        let metrics = workers.pop().unwrap();
        let worker = workers.pop().unwrap();
        worker.set_gauge(Gauge::KeyringGeneration, 3);
        worker.set_gauge(Gauge::IdentityExpiresAtSeconds, 120);
        worker.set_gauge(Gauge::IdentityExpiresAtSeconds, 200);
        assert_eq!(metrics.gauge(Gauge::KeyringGeneration), 3);
        assert_eq!(metrics.gauge(Gauge::IdentityExpiresAtSeconds), 200);
        let mut output = String::new();
        metrics.write_prometheus(&mut output).unwrap();
        assert!(output.contains("racer_keyring_generation 3\n"));
        assert!(output.contains("racer_identity_expires_at_seconds 200\n"));
    }

    #[test]
    fn fixed_registry_is_aligned_and_clones_keep_their_writer() {
        assert!(matches!(
            Metrics::for_workers(0),
            Err(crate::error::Error::InvalidConfiguration)
        ));
        let workers = Metrics::for_workers(3).unwrap();
        assert_eq!(std::mem::align_of::<Counters>(), 64);
        assert_eq!(std::mem::size_of::<Counters>() % 64, 0);
        assert_eq!(std::mem::size_of::<GaugeCounter>(), 64);
        for (index, worker) in workers.iter().enumerate() {
            assert_eq!(worker.shard, index);
            let clone = worker.clone();
            assert_eq!(clone.shard, index);
            assert!(Arc::ptr_eq(&clone.registry, &workers[0].registry));
            clone.record(Event::MemoryHit, (index + 1) as u64).unwrap();
            let shard = &worker.registry.shards[index];
            assert_eq!(std::ptr::from_ref(shard) as usize % 64, 0);
            assert_eq!(
                shard.events[Event::MemoryHit as usize].load(Ordering::Relaxed),
                (index + 1) as u64
            );
        }
        assert_eq!(workers[0].count(Event::MemoryHit), 6);
    }

    #[test]
    fn concurrent_writers_and_scrapes_retain_totals_after_worker_exit() {
        let workers = Metrics::for_workers(4).unwrap();
        let reader = workers[0].clone();
        let barrier = Arc::new(std::sync::Barrier::new(workers.len() + 1));
        std::thread::scope(|scope| {
            for worker in workers {
                let barrier = barrier.clone();
                scope.spawn(move || {
                    barrier.wait();
                    for _ in 0..1000 {
                        let mut request = worker.request().unwrap();
                        worker.record(Event::MemoryHit, 1).unwrap();
                        request.success();
                    }
                });
            }
            barrier.wait();
            let mut last = 0;
            for _ in 0..100 {
                let count = reader.count(Event::Request);
                assert!((last..=4000).contains(&count));
                last = count;
                let mut output = String::new();
                reader.write_prometheus(&mut output).unwrap();
                assert_eq!(
                    output.lines().filter(|line| !line.starts_with('#')).count(),
                    EVENT_COUNT + GAUGE_COUNT
                );
                assert!(!output.contains('{'));
            }
        });
        assert_eq!(reader.count(Event::Request), 4000);
        assert_eq!(reader.count(Event::MemoryHit), 4000);
        assert_eq!(reader.count(Event::RequestError), 0);
        assert_eq!(reader.gauge(Gauge::ActiveRequests), 0);
    }

    #[test]
    fn aggregate_events_saturate_without_wrapping() {
        let workers = Metrics::for_workers(2).unwrap();
        for event in EVENTS {
            workers[0].record(event, u64::MAX - 1).unwrap();
            workers[1].record(event, 2).unwrap();
            assert_eq!(workers[0].count(event), u64::MAX);
            workers[1].record(event, u64::MAX).unwrap();
            assert_eq!(workers[1].count(event), u64::MAX);
        }
    }

    #[test]
    fn gauges_preserve_node_wide_overflow_replacement_and_cross_thread_release() {
        let workers = Metrics::for_workers(2).unwrap();
        for gauge in GAUGES {
            workers[0].set_gauge(gauge, u64::MAX - 1);
            let lease = workers[1].lease(gauge).unwrap();
            assert_eq!(workers[0].gauge(gauge), u64::MAX);
            assert!(matches!(
                workers[0].lease(gauge),
                Err(crate::error::Error::Overloaded)
            ));
            assert!(matches!(
                workers[1].lease(gauge),
                Err(crate::error::Error::Overloaded)
            ));
            std::thread::spawn(move || drop(lease)).join().unwrap();
            assert_eq!(workers[0].gauge(gauge), u64::MAX - 1);
            workers[1].set_gauge(gauge, 0);
            workers[0].add_gauge(gauge, 2);
            workers[1].add_gauge(gauge, 3);
            assert_eq!(workers[0].gauge(gauge), 5);
        }
        let lease = workers[1].lease(Gauge::ActiveRequests).unwrap();
        let reader = workers[0].clone();
        drop(workers);
        std::thread::spawn(move || drop(lease)).join().unwrap();
        assert_eq!(reader.gauge(Gauge::ActiveRequests), 5);
    }
}
