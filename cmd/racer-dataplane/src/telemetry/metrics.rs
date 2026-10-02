//! Fixed metric names; only installed runtime worker IDs are exposed as labels.
//!
//! Integrity diagnostics are two independent axes, not a reason/source matrix:
//! - `racer_crypto_decrypt_{crc,aead}_rejected_total` counts the exact failed
//!   check once at completion reap, including abandoned jobs. Structural errors,
//!   missing keys, and cancellation are not classified as CRC or AEAD failures.
//! - `racer_fill_decrypt_{disk,retained,peer}_corrupt_total` counts CorruptRecord
//!   results observed by the fill decrypt helper (including its structural checks).
//!   Retained includes memory, flights, and pending writes, even for copies first
//!   read from disk or peers. Earlier read/response parsing failures are excluded.
//! These counters count attempts, not unique records or corrupt client deliveries,
//! and identify rejection/check location, not where corruption originated. Source
//! totals need not equal crypto totals, especially with abandoned fill waiters.
use crate::{
    error::Result,
    model::WorkerId,
    runtime::admission::{AdmissionPolicy, SharedAdmissionExt},
};
use ::telemetry::metrics;
use std::sync::{Arc, OnceLock};

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
    core: ::telemetry::Metrics<Event, Gauge>,
    admission: Arc<[OnceLock<(WorkerId, flow_control::SharedQuotas<AdmissionPolicy>)>]>,
    shard: usize,
}

impl Default for Metrics {
    fn default() -> Self {
        Self::for_workers(1)
            .expect("one metrics worker")
            .pop()
            .unwrap()
    }
}
// Declaration order is the counter index; names are the exported wire contract.
metrics! { Event, EVENTS, EVENT_COUNT;
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
            Self::PeerPageCheckoutCount => "racer_peer_page_checkout_nanoseconds_count",
            Self::PeerPageCheckoutNs => "racer_peer_page_checkout_nanoseconds_sum",
            Self::PeerPageAuthCount => "racer_peer_page_auth_nanoseconds_count",
            Self::PeerPageAuthNs => "racer_peer_page_auth_nanoseconds_sum",
            Self::PeerPageHeadCount => "racer_peer_page_head_nanoseconds_count",
            Self::PeerPageHeadNs => "racer_peer_page_head_nanoseconds_sum",
            Self::PeerPageBodyCount => "racer_peer_page_body_nanoseconds_count",
            Self::PeerPageBodyNs => "racer_peer_page_body_nanoseconds_sum",
            Self::PeerPageCensored => "racer_peer_page_censored_total",
            Self::CryptoDecryptCrcRejected => "racer_crypto_decrypt_crc_rejected_total",
            Self::CryptoDecryptAeadRejected => "racer_crypto_decrypt_aead_rejected_total",
            Self::FillDecryptDiskCorrupt => "racer_fill_decrypt_disk_corrupt_total",
            Self::FillDecryptRetainedCorrupt => "racer_fill_decrypt_retained_corrupt_total",
            Self::FillDecryptPeerCorrupt => "racer_fill_decrypt_peer_corrupt_total",
            Self::OpaqueRelayBodyCompleted => "racer_opaque_relay_body_completed_total",
            Self::OpaqueRelayBodyBytes => "racer_opaque_relay_body_completed_bytes_total",
            Self::OpaqueRelayBodyFailed => "racer_opaque_relay_body_failed_total",
}
metrics! { Gauge, GAUGES, GAUGE_COUNT;
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
/// Keep with the actual resource, including through a submitted I/O fence.
pub type GaugeLease = ::telemetry::Lease;
/// One nonempty intermediate HTTP relay_body call, after sending its response head.
/// Success credits the entire ciphertext body only after both HTTP finish checks.
/// Error or abandonment credits one failure and no bytes, even after partial writes.
/// Head failures, empty bodies, materialized/native paths, and pre-body retries are
/// excluded. Splice-to-copy fallback is still one attempt. These are per-hop transfer
/// counts, not unique pages, endpoint AEAD acceptance, or confirmed client delivery.
pub(crate) struct OpaqueRelayBody<'a> {
    metrics: &'a Metrics,
    bytes: usize,
    completed: bool,
}
impl OpaqueRelayBody<'_> {
    pub(crate) fn complete(&mut self) {
        self.completed = true;
    }
}
impl Drop for OpaqueRelayBody<'_> {
    fn drop(&mut self) {
        if self.completed {
            let _ = self
                .metrics
                .record(Event::OpaqueRelayBodyBytes, self.bytes as u64);
            let _ = self.metrics.record(Event::OpaqueRelayBodyCompleted, 1);
        } else {
            let _ = self.metrics.record(Event::OpaqueRelayBodyFailed, 1);
        }
    }
}
/// One complete client head through final delivery, including cancellation/drop.
pub(crate) struct RequestMetrics {
    _active: GaugeLease,
    metrics: Metrics,
    succeeded: bool,
    overloaded: bool,
}
impl RequestMetrics {
    pub(crate) fn success(&mut self) {
        self.succeeded = true;
    }
    pub(crate) fn fail(&mut self, error: crate::error::Error) {
        if error == crate::error::Error::Overloaded && !self.overloaded {
            let _ = self.metrics.record(Event::Overload, 1);
            self.overloaded = true;
        }
    }
}
impl Drop for RequestMetrics {
    fn drop(&mut self) {
        if !self.succeeded {
            let _ = self.metrics.record(Event::RequestError, 1);
        }
    }
}
impl Metrics {
    pub(crate) fn opaque_relay_body(&self, bytes: usize) -> Option<OpaqueRelayBody<'_>> {
        (bytes != 0).then(|| OpaqueRelayBody {
            metrics: self,
            bytes,
            completed: false,
        })
    }
    /// Install once during worker assembly, not on the admission hot path.
    pub(crate) fn observe_admission(
        &self,
        worker: WorkerId,
        usage: flow_control::SharedQuotas<AdmissionPolicy>,
    ) -> Result<()> {
        self.admission[self.shard]
            .set((worker, usage))
            .map_err(|_| crate::error::Error::InvalidConfiguration)
    }
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
        let admission: Arc<[_]> = (0..count).map(|_| OnceLock::new()).collect();
        Ok(::telemetry::Metrics::shards(count)
            .into_iter()
            .enumerate()
            .map(|(shard, core)| Self {
                core,
                admission: admission.clone(),
                shard,
            })
            .collect())
    }

    pub(crate) fn request(&self) -> Result<RequestMetrics> {
        let active = self.lease(Gauge::ActiveRequests)?;
        self.record(Event::Request, 1)?;
        Ok(RequestMetrics {
            _active: active,
            metrics: self.clone(),
            succeeded: false,
            overloaded: false,
        })
    }
    /// Saturate instead of wrapping a long-lived Prometheus counter.
    pub fn record(&self, event: Event, amount: u64) -> Result<()> {
        self.core.add(event, amount);
        Ok(())
    }
    pub fn count(&self, event: Event) -> u64 {
        self.core.count(event)
    }
    pub fn gauge(&self, gauge: Gauge) -> u64 {
        self.core.gauge(gauge)
    }
    pub(crate) fn set_gauge(&self, gauge: Gauge, value: u64) {
        self.core.set(gauge, value);
    }
    pub(crate) fn add_gauge(&self, gauge: Gauge, value: u64) {
        self.core.increase(gauge, value);
    }
    pub fn lease(&self, gauge: Gauge) -> Result<GaugeLease> {
        self.core
            .lease(gauge)
            .ok_or(crate::error::Error::Overloaded)
    }
    /// The destination is bounded by the diagnostic server; no intermediate String.
    /// Relaxed per-series observations are not a coherent snapshot of all workers.
    pub fn write_prometheus(&self, out: &mut impl std::fmt::Write) -> std::fmt::Result {
        self.core.write_prometheus(out)?;
        // Only runtime worker IDs are labels. Read the authority's actual charge,
        // including pooled ciphertext capacity, without sampling on worker polls.
        // A stalled worker therefore remains observable from another worker.
        const QUOTAS: [&str; 4] = [
            "racer_worker_relay_used",
            "racer_worker_relay_limit",
            "racer_worker_ciphertext_used_bytes",
            "racer_worker_ciphertext_limit_bytes",
        ];
        if self.admission.iter().any(|s| s.get().is_some()) {
            for name in QUOTAS {
                writeln!(out, "# TYPE {name} gauge")?;
            }
            for shard in self.admission.iter() {
                if let Some((worker, usage)) = shard.get() {
                    let (relay_used, relay_limit) = usage.relay();
                    let (ciphertext_used, ciphertext_limit) = usage.ciphertext();
                    for (name, value) in QUOTAS.into_iter().zip([
                        relay_used,
                        relay_limit,
                        ciphertext_used,
                        ciphertext_limit,
                    ]) {
                        writeln!(out, "{name}{{worker=\"{}\"}} {value}", worker.0)?;
                    }
                }
            }
        }
        Ok(())
    }
}
#[cfg(test)]
mod tests {
    use super::*;
    use crate::{model::ResourceClass, runtime::admission::AdmissionExt};

    #[test]
    fn request_lease_overflow_preserves_counters_and_error() {
        let metrics = Metrics::default();
        metrics.set_gauge(Gauge::ActiveRequests, u64::MAX);
        assert!(matches!(
            metrics.request(),
            Err(crate::error::Error::Overloaded)
        ));
        assert_eq!(metrics.count(Event::Request), 0);
        assert_eq!(metrics.count(Event::RequestError), 0);
        assert_eq!(metrics.gauge(Gauge::ActiveRequests), u64::MAX);
    }

    #[test]
    fn opaque_body_attempts_exclude_empty_and_count_abandonment_once() {
        let metrics = Metrics::default();
        assert!(metrics.opaque_relay_body(0).is_none());
        assert_eq!(metrics.count(Event::OpaqueRelayBodyFailed), 0);
        let mut completed = metrics.opaque_relay_body(17).unwrap();
        assert_eq!(metrics.count(Event::OpaqueRelayBodyBytes), 0);
        completed.complete();
        completed.complete();
        drop(completed);
        drop(metrics.opaque_relay_body(19));
        assert_eq!(metrics.count(Event::OpaqueRelayBodyCompleted), 1);
        assert_eq!(metrics.count(Event::OpaqueRelayBodyBytes), 17);
        assert_eq!(metrics.count(Event::OpaqueRelayBodyFailed), 1);
    }

    #[test]
    fn integrity_diagnostics_have_fixed_names_and_saturating_sharded_counts() {
        let workers = Metrics::for_workers(2).unwrap();
        let events = [
            Event::CryptoDecryptCrcRejected,
            Event::CryptoDecryptAeadRejected,
            Event::FillDecryptDiskCorrupt,
            Event::FillDecryptRetainedCorrupt,
            Event::FillDecryptPeerCorrupt,
        ];
        for event in events {
            workers[0].record(event, u64::MAX).unwrap();
            workers[0].record(event, 1).unwrap();
            workers[1].record(event, 1).unwrap();
            assert_eq!(workers[1].count(event), u64::MAX);
        }
        let mut output = String::new();
        workers[1].write_prometheus(&mut output).unwrap();
        for event in events {
            assert!(output.contains(&format!(
                "# TYPE {} counter\n{} {}\n",
                event.name(),
                event.name(),
                u64::MAX
            )));
        }
        assert!(!output.contains('{'), "no identity or content labels");
        assert_eq!(
            EVENTS
                .iter()
                .map(|e| e.name())
                .collect::<std::collections::BTreeSet<_>>()
                .len(),
            EVENT_COUNT
        );
    }

    #[test]
    fn worker_quota_gauges_follow_authoritative_reservations() {
        let workers = Metrics::for_workers(2).unwrap();
        let admission = flow_control::Quotas::new(AdmissionPolicy::new(
            crate::test_support::cluster::config(false).limits,
        ));
        let other = flow_control::Quotas::new(AdmissionPolicy::new(
            crate::test_support::cluster::config(false).limits,
        ));
        workers[0]
            .observe_admission(WorkerId(7), admission.usage())
            .unwrap();
        workers[1]
            .observe_admission(WorkerId(19), other.usage())
            .unwrap();
        assert!(matches!(
            workers[0].observe_admission(WorkerId(20), other.usage()),
            Err(crate::error::Error::InvalidConfiguration)
        ));
        let scrape = || {
            let mut output = String::new();
            workers[1].write_prometheus(&mut output).unwrap();
            output
        };
        let relay_limit = admission.limit(ResourceClass::Relay);
        let ciphertext_limit = admission.limit(ResourceClass::Ciphertext);
        let idle = scrape();
        for worker in [7, 19] {
            for (name, value) in [
                ("relay_used", 0),
                ("relay_limit", relay_limit),
                ("ciphertext_used_bytes", 0),
                ("ciphertext_limit_bytes", ciphertext_limit),
            ] {
                assert!(idle.contains(&format!(
                    "racer_worker_{name}{{worker=\"{worker}\"}} {value}\n"
                )));
            }
        }
        let relay = admission
            .reserve(None, ResourceClass::Relay, relay_limit)
            .unwrap();
        let mut ciphertext = admission
            .reserve(None, ResourceClass::Ciphertext, ciphertext_limit)
            .unwrap();
        let full = scrape();
        for (name, value) in [
            ("relay_used", relay_limit),
            ("ciphertext_used_bytes", ciphertext_limit),
        ] {
            assert!(full.contains(&format!("racer_worker_{name}{{worker=\"7\"}} {value}\n")));
            assert!(full.contains(&format!("racer_worker_{name}{{worker=\"19\"}} 0\n")));
        }
        for class in [ResourceClass::Relay, ResourceClass::Ciphertext] {
            assert!(matches!(
                admission.reserve(None, class, 1),
                Err(flow_control::Error::Overloaded)
            ));
        }
        assert_eq!(scrape(), full, "rejection must not change quota gauges");
        let split = ciphertext.split(8).unwrap();
        assert_eq!(scrape(), full, "splitting retains the total charge");
        drop(split);
        ciphertext.shrink(16).unwrap();
        assert!(scrape().contains("racer_worker_ciphertext_used_bytes{worker=\"7\"} 16\n"));
        admission.stop();
        drop(admission);
        assert!(scrape().contains("racer_worker_ciphertext_used_bytes{worker=\"7\"} 16\n"));
        std::thread::spawn(move || drop((relay, ciphertext)))
            .join()
            .unwrap();
        assert_eq!(
            scrape(),
            idle,
            "final release remains visible after owner exit"
        );
        assert_eq!(
            idle.lines()
                .filter(|line| line.contains("{worker="))
                .count(),
            8
        );
        assert!(!idle.contains("request="));
    }

    #[test]
    fn worker_quota_gauges_include_recycled_ciphertext_capacity() {
        let metrics = Metrics::default();
        let admission = flow_control::Quotas::new(AdmissionPolicy::new(
            crate::test_support::cluster::config(false).limits,
        ));
        metrics
            .observe_admission(WorkerId(0), admission.usage())
            .unwrap();
        let mut reservation = admission
            .reserve(None, ResourceClass::Ciphertext, 1 << 20)
            .unwrap();
        let bytes = reservation.buffer(1 << 20).unwrap();
        reservation.recycle(bytes);
        drop(reservation);
        let mut output = String::new();
        metrics.write_prometheus(&mut output).unwrap();
        assert!(output.contains("racer_worker_ciphertext_used_bytes{worker=\"0\"} 1048576\n"));
        admission.stop();
        output.clear();
        metrics.write_prometheus(&mut output).unwrap();
        assert!(output.contains("racer_worker_ciphertext_used_bytes{worker=\"0\"} 0\n"));
    }

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
        assert!(output.len() < 64 * 1024);
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
        // Cache-line alignment and per-shard storage assertions live in the core
        // test of the same name; this adapter retains its worker mapping checks.
        for (index, worker) in workers.iter().enumerate() {
            assert_eq!(worker.shard, index);
            let clone = worker.clone();
            assert_eq!(clone.shard, index);
            assert!(Arc::ptr_eq(&clone.admission, &workers[0].admission));
            clone.record(Event::MemoryHit, (index + 1) as u64).unwrap();
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
