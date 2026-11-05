use super::*;
use crate::metrics;

// Fixed metric names; only installed runtime worker IDs are exposed as labels.
//
// Integrity diagnostics are two independent axes, not a reason/source matrix:
// - `racer_crypto_decrypt_aead_rejected_total` counts the exact failed check
//   once at completion reap, including abandoned jobs. Structural errors,
//   missing keys, and cancellation are not classified as AEAD failures.
// - `racer_fill_decrypt_{disk,retained,peer}_corrupt_total` counts CorruptRecord
//   results observed by the fill decrypt helper (including its structural checks).
//   Retained includes memory, flights, and pending writes, even for copies first
//   read from disk or peers. Earlier read/response parsing failures are excluded.
// These counters count attempts, not unique records or corrupt client deliveries,
// and identify rejection/check location, not where corruption originated. Source
// totals need not equal crypto totals, especially with abandoned fill waiters.

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
    core: crate::telemetry::primitives::Metrics<Event, Gauge>,

    // Each registration pairs its worker identity with the shared quota view.
    #[allow(clippy::type_complexity)]
    admission: Arc<[OnceLock<(WorkerId, flow_control::SharedQuotas<AdmissionPolicy>)>]>,

    retention: crate::telemetry::primitives::SnapshotShards<crate::retention::Snapshot>,

    held: crate::telemetry::primitives::SnapshotShards<crate::store::catalog::HeldSnapshot>,

    disks: crate::telemetry::primitives::SnapshotShards<Vec<DiskUsage>>,

    memory: Arc<MemoryShards>,
    cache_disks: crate::telemetry::primitives::SnapshotShards<Vec<CacheDiskUsage>>,

    /// GPU, cuFile, and GPU landing sources, shared by every worker.
    gpu: Arc<crate::gpu_telemetry::GpuTelemetry>,

    /// This worker's handle on the prefetch registry.
    prefetch: Arc<crate::prefetch::metrics::PrefetchMetrics>,

    shard: usize,
}

/// Memory telemetry shared by every worker's handle, behind one pointer.
struct MemoryShards {
    staging: crate::telemetry::primitives::SnapshotShards<Option<(WorkerId, flow_control::PoolStats)>>,

    // One node-wide slot written by the sampling worker.
    process: crate::telemetry::primitives::SnapshotShards<ProcessMemory>,
}

/// Process and cgroup memory signals from the most recent sample.
/// Absent fields are omitted from the exposition.
#[derive(Clone, Copy, Debug, Default, PartialEq)]
pub(crate) struct ProcessMemory {
    pub resident: Option<u64>,
    pub cgroup_current: Option<u64>,
    pub cgroup_limit: Option<u64>,
    pub pressure_some_avg10: Option<f64>,
}

impl From<&crate::pressure::MemorySample> for ProcessMemory {
    fn from(sample: &crate::pressure::MemorySample) -> Self {
        Self {
            resident: sample.resident,
            cgroup_current: sample.current,
            cgroup_limit: sample.limit,
            pressure_some_avg10: sample.some_avg10,
        }
    }
}

/// Snake-case `class` label values for every admission class, in index order.
const ADMISSION_CLASSES: [(crate::admission::ResourceClass, &str); 17] = {
    use crate::admission::ResourceClass as C;
    [
        (C::Plaintext, "plaintext"),
        (C::Ciphertext, "ciphertext"),
        (C::DirtyCiphertext, "dirty_ciphertext"),
        (C::Registered, "registered"),
        (C::RequestContext, "request_context"),
        (C::Flight, "flight"),
        (C::Waiter, "waiter"),
        (C::Connection, "connection"),
        (C::Pipe, "pipe"),
        (C::ControlProgress, "control_progress"),
        (C::Relay, "relay"),
        (C::IngressConnection, "ingress_connection"),
        (C::OutboundConnection, "outbound_connection"),
        (C::ControlConnection, "control_connection"),
        (
            C::OrdinaryOutboundConnection,
            "ordinary_outbound_connection",
        ),
        (C::PeerIngressConnection, "peer_ingress_connection"),
        (C::Pool, "pool"),
    ]
};

/// Node-wide idle pool series: name suffix, Prometheus kind, and value.
fn pool_series(stats: &flow_control::PoolStats) -> [(&'static str, &'static str, u64); 7] {
    [
        ("entries", "gauge", stats.entries as u64),
        ("bytes", "gauge", stats.bytes as u64),
        ("target", "gauge", stats.target as u64),
        ("hits_total", "counter", stats.hits),
        ("misses_total", "counter", stats.misses),
        ("rejects_total", "counter", stats.rejects),
        ("trimmed_total", "counter", stats.trimmed),
    ]
}

/// Write node-wide sums as `racer_{family}_*` and only retained bytes per
/// worker as `racer_worker_{family}_bytes{worker="N"}`. Per-worker breakdowns of
/// every series would not fit the bounded diagnostic response on large nodes.
fn write_pools(
    out: &mut impl Write,
    family: &str,
    samples: &[(WorkerId, flow_control::PoolStats)],
) -> std::fmt::Result {
    if samples.is_empty() {
        return Ok(());
    }
    let mut totals = [0u64; 7];
    for (_, stats) in samples {
        for (total, (_, _, value)) in totals.iter_mut().zip(pool_series(stats)) {
            *total = total.saturating_add(value);
        }
    }
    for ((suffix, kind, _), total) in pool_series(&Default::default()).into_iter().zip(totals) {
        writeln!(
            out,
            "# TYPE racer_{family}_{suffix} {kind}\nracer_{family}_{suffix} {total}"
        )?;
    }
    writeln!(out, "# TYPE racer_worker_{family}_bytes gauge")?;
    for (worker, stats) in samples {
        writeln!(
            out,
            "racer_worker_{family}_bytes{{worker=\"{}\"}} {}",
            worker.0, stats.bytes
        )?;
    }
    Ok(())
}

#[derive(Clone)]
/// One worker's indexed payload and fair disk share for a published cache.
pub(crate) struct CacheDiskUsage {
    pub cache: Arc<str>,
    pub resident: u64,
    pub share: u64,
}

#[derive(Clone)]
/// One worker's assigned capacity and reserved record bytes on a backing disk.
pub(crate) struct DiskUsage {
    pub disk: Arc<str>,
    pub size: u64,
    pub used: u64,
}

fn write_disk_label(out: &mut impl Write, disk: &str) -> std::fmt::Result {
    for ch in disk.chars() {
        match ch {
            '\\' => out.write_str("\\\\")?,
            '"' => out.write_str("\\\"")?,
            '\n' => out.write_str("\\n")?,
            _ => out.write_char(ch)?,
        }
    }
    Ok(())
}
pub(super) const RETENTION_METRICS: [(&str, &str); 9] = [
    ("racer_retention_observations_total", "counter"),
    ("racer_retention_qualified_total", "counter"),
    ("racer_retention_persistence_attempts_total", "counter"),
    ("racer_retention_persistence_accepted_total", "counter"),
    ("racer_retention_filter_set_bits", "gauge"),
    ("racer_retention_filter_bits", "gauge"),
    ("racer_disk_pending_payload_bytes", "gauge"),
    ("racer_disk_indexed_payload_bytes", "gauge"),
    ("racer_retention_share_rejected_total", "counter"),
];
const CACHE_DISK_METRICS: [&str; 2] = [
    "racer_cache_disk_resident_bytes",
    "racer_cache_disk_share_bytes",
];
pub(super) const HELD_METRICS: [(&str, &str); 3] = [
    ("racer_held_pages", "gauge"),
    ("racer_held_payload_bytes", "gauge"),
    ("racer_held_segments", "gauge"),
];
const DISK_CLASS_METRICS: [&str; 5] = [
    "racer_disk_class_publications_total",
    "racer_disk_class_published_payload_bytes_total",
    "racer_disk_class_segment_evicted_pages_total",
    "racer_disk_class_segment_evicted_payload_bytes_total",
    "racer_disk_class_read_payload_bytes_total",
];

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
            Self::PeerReceiveAdmitted => "racer_peer_receive_admitted_total",
            Self::PeerReceiveFull => "racer_peer_receive_full_total",
            Self::PeerReceiveTimeout => "racer_peer_receive_timeout_total",
            Self::PeerReceiveCancelled => "racer_peer_receive_canceled_total",
            Self::PeerReceiveWaitNs => "racer_peer_receive_wait_ns_total",
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
            Self::RequestErrorOverloaded => "racer_request_errors_overloaded_total",
            Self::RequestErrorUnavailable => "racer_request_errors_unavailable_total",
            Self::RequestErrorDeadline => "racer_request_errors_deadline_total",
            Self::RequestErrorCancelled => "racer_request_errors_cancelled_total",
            Self::RequestErrorOther => "racer_request_errors_other_total",
            Self::MemoryHit => "racer_memory_hits_total",
            Self::DiskHit => "racer_disk_hits_total",
            Self::PeerHit => "racer_peer_hits_total",
            Self::OriginFill => "racer_origin_fills_total",
            Self::DirtyDiscard => "racer_dirty_discards_total",
            Self::DiskPublication => "racer_disk_publications_total",
            Self::PersistZeroCopy => "racer_persist_zero_copy_total",
            Self::PersistCopyFallback => "racer_persist_copy_fallback_total",
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
            Self::DeliveryBytes => "racer_delivery_bytes_total",
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
            Self::CryptoDecryptAeadRejected => "racer_crypto_decrypt_aead_rejected_total",
            Self::FillDecryptDiskCorrupt => "racer_fill_decrypt_disk_corrupt_total",
            Self::FillDecryptRetainedCorrupt => "racer_fill_decrypt_retained_corrupt_total",
            Self::FillDecryptPeerCorrupt => "racer_fill_decrypt_peer_corrupt_total",
            Self::OpaqueRelayBodyCompleted => "racer_opaque_relay_body_completed_total",
            Self::OpaqueRelayBodyBytes => "racer_opaque_relay_body_completed_bytes_total",
            Self::OpaqueRelayBodyFailed => "racer_opaque_relay_body_failed_total",
            Self::CryptoDecryptFailureCancelled => "racer_crypto_decrypt_failure_cancelled_total",
            Self::CryptoDecryptFailureDeadlineExceeded => "racer_crypto_decrypt_failure_deadline_exceeded_total",
            Self::CryptoDecryptFailureMissingKey => "racer_crypto_decrypt_failure_missing_key_total",
            Self::CryptoDecryptFailureInvalidConfiguration => "racer_crypto_decrypt_failure_invalid_configuration_total",
            Self::CryptoDecryptFailureInvalidRequest => "racer_crypto_decrypt_failure_invalid_request_total",
            Self::CryptoDecryptFailureOverloaded => "racer_crypto_decrypt_failure_overloaded_total",
            Self::CryptoDecryptFailureCorruptAead => "racer_crypto_decrypt_failure_corrupt_aead_total",
            Self::CryptoDecryptFailureCorruptUnclassified => "racer_crypto_decrypt_failure_corrupt_unclassified_total",
            Self::CryptoDecryptFailureOther => "racer_crypto_decrypt_failure_other_total",
            Self::MemoryPressureTransitions => "racer_memory_pressure_transitions_total",
            Self::AllocatorTrims => "racer_allocator_trims_total",
            Self::RdmaPagesSent => "racer_rdma_pages_sent_total",
            Self::RdmaSentBytes => "racer_rdma_sent_bytes_total",
            Self::RdmaPagesReceived => "racer_rdma_pages_received_total",
            Self::RdmaReceivedBytes => "racer_rdma_received_bytes_total",
            Self::RdmaPagesLanded => "racer_rdma_pages_landed_total",
            Self::RdmaHttpFallback => "racer_rdma_http_fallback_total",
            Self::RdmaFallbackNoLink => "racer_rdma_fallback_no_link_total",
            Self::RdmaFallbackGrant => "racer_rdma_fallback_grant_total",
            Self::RdmaFallbackOverflow => "racer_rdma_fallback_overflow_total",
            Self::RdmaActivationFailure => "racer_rdma_activation_failures_total",
            Self::RdmaGrantCount => "racer_rdma_grant_nanoseconds_count",
            Self::RdmaGrantNs => "racer_rdma_grant_nanoseconds_sum",
            Self::RdmaHeadCount => "racer_rdma_head_nanoseconds_count",
            Self::RdmaHeadNs => "racer_rdma_head_nanoseconds_sum",
            Self::RdmaArrivalCount => "racer_rdma_arrival_nanoseconds_count",
            Self::RdmaArrivalNs => "racer_rdma_arrival_nanoseconds_sum",
            Self::RdmaSendCount => "racer_rdma_send_nanoseconds_count",
            Self::RdmaSendNs => "racer_rdma_send_nanoseconds_sum",
            Self::RdmaServeCount => "racer_rdma_serve_nanoseconds_count",
            Self::RdmaServeNs => "racer_rdma_serve_nanoseconds_sum",
            Self::FillDriverStartCount => "racer_fill_driver_start_nanoseconds_count",
            Self::FillDriverStartNs => "racer_fill_driver_start_nanoseconds_sum",
            Self::FillDriverCount => "racer_fill_driver_nanoseconds_count",
            Self::FillDriverNs => "racer_fill_driver_nanoseconds_sum",
            Self::FillCandidatesCount => "racer_fill_candidates_nanoseconds_count",
            Self::FillCandidatesNs => "racer_fill_candidates_nanoseconds_sum",
            Self::FillPlaintextReclaimCount => "racer_fill_plaintext_reclaim_nanoseconds_count",
            Self::FillPlaintextReclaimNs => "racer_fill_plaintext_reclaim_nanoseconds_sum",
            Self::FillPeerResponseCount => "racer_fill_peer_response_nanoseconds_count",
            Self::FillPeerResponseNs => "racer_fill_peer_response_nanoseconds_sum",
            Self::FillPeerDecryptCount => "racer_fill_peer_decrypt_nanoseconds_count",
            Self::FillPeerDecryptNs => "racer_fill_peer_decrypt_nanoseconds_sum",
            Self::FillHedgeCount => "racer_fill_hedge_nanoseconds_count",
            Self::FillHedgeNs => "racer_fill_hedge_nanoseconds_sum",
            Self::FillPublishCount => "racer_fill_publish_nanoseconds_count",
            Self::FillPublishNs => "racer_fill_publish_nanoseconds_sum",
            Self::FillDecryptCount => "racer_fill_decrypt_nanoseconds_count",
            Self::FillDecryptNs => "racer_fill_decrypt_nanoseconds_sum",
            Self::FillDiskReadCount => "racer_fill_disk_read_nanoseconds_count",
            Self::FillDiskReadNs => "racer_fill_disk_read_nanoseconds_sum",
            Self::FillOriginCount => "racer_fill_origin_nanoseconds_count",
            Self::FillOriginNs => "racer_fill_origin_nanoseconds_sum",
            Self::FillEncryptCount => "racer_fill_encrypt_nanoseconds_count",
            Self::FillEncryptNs => "racer_fill_encrypt_nanoseconds_sum",
            Self::FillTagCheckCount => "racer_fill_tag_check_nanoseconds_count",
            Self::FillTagCheckNs => "racer_fill_tag_check_nanoseconds_sum",
            Self::DiagnosticHardware => "racer_diagnostic_hardware_total",
            Self::GpuDirectDiskRead => "racer_gpu_direct_disk_reads_total",
            Self::GpuDirectDiskReadBytes => "racer_gpu_direct_disk_read_bytes_total",
            Self::SuccessorHint => "racer_successor_hints_total",
            Self::SuccessorDeferral => "racer_successor_deferrals_total",
            Self::CopyJoinDeclined => "racer_copy_join_declined_total",
            Self::OrphanNotice => "racer_orphan_notices_total",
            Self::OrphanOfferSent => "racer_orphan_offers_sent_total",
            Self::OrphanOfferDelivered => "racer_orphan_offers_delivered_total",
            Self::OrphanOfferFailed => "racer_orphan_offers_failed_total",
            Self::OrphanOfferNoted => "racer_orphan_offers_noted_total",
            Self::SilencePingSent => "racer_silence_pings_sent_total",
            Self::SilencePingAnswered => "racer_silence_pings_answered_total",
            Self::SilencePeerSilent => "racer_silence_peers_silent_total",
            Self::GpuDirectJoined => "racer_gpu_direct_joined_total",
            Self::PassthroughLocalOrigin => "racer_passthrough_local_origin_fills_total",
            Self::GpuReadGpu => "racer_gpu_read_requests_total" { "outcome=\"gpu\"" },
            Self::GpuReadNotEnabled => "racer_gpu_read_requests_total" { "outcome=\"not_enabled\"" },
            Self::GpuReadWrongDevice => "racer_gpu_read_requests_total" { "outcome=\"wrong_device\"" },
            Self::GpuReadBusy => "racer_gpu_read_requests_total" { "outcome=\"busy\"" },
            Self::GpuReadInvalid => "racer_gpu_read_requests_total" { "outcome=\"invalid\"" },
            Self::GpuReadFailed => "racer_gpu_read_requests_total" { "outcome=\"failed\"" },
}

/// The labeled request counter for one `gpu-read` outcome.
fn gpu_read_event(outcome: crate::gpu_telemetry::GpuReadOutcome) -> Event {
    use crate::gpu_telemetry::GpuReadOutcome as O;
    match outcome {
        O::Gpu => Event::GpuReadGpu,
        O::NotEnabled => Event::GpuReadNotEnabled,
        O::WrongDevice => Event::GpuReadWrongDevice,
        O::Busy => Event::GpuReadBusy,
        O::Invalid => Event::GpuReadInvalid,
        O::Failed => Event::GpuReadFailed,
    }
}
metrics! { Gauge, GAUGES, GAUGE_COUNT;
            Self::PeerReceiveActive => "racer_peer_receive_active",
            Self::PeerReceiveQueued => "racer_peer_receive_queued",
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
            Self::MemoryPressureLevel => "racer_memory_pressure_level",
            Self::RdmaFreeBuffers => "racer_rdma_free_buffers",
            Self::RdmaActive => "racer_rdma_active",
}
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
            self.metrics
                .record(Event::OpaqueRelayBodyBytes, self.bytes as u64);
            self.metrics.record(Event::OpaqueRelayBodyCompleted, 1);
        } else {
            self.metrics.record(Event::OpaqueRelayBodyFailed, 1);
        }
    }
}
/// One complete client head through final delivery, including cancellation/drop.
/// Holds one worker-local lease, which is also its counter handle, so opening
/// a request clones a single pointer and touches only this worker's lines.
pub(crate) struct RequestMetrics {
    active: crate::telemetry::primitives::LocalLease<Event>,
    succeeded: bool,
    overloaded: bool,
    /// The first reported failure, which names why the request ended.
    failure: Option<crate::error::Error>,
}
impl RequestMetrics {
    pub(crate) fn success(&mut self) {
        self.succeeded = true;
    }
    pub(crate) fn fail(&mut self, error: crate::error::Error) {
        if error == crate::error::Error::Overloaded && !self.overloaded {
            self.active.add(Event::Overload, 1);
            self.overloaded = true;
        }
        self.failure.get_or_insert(error);
    }
}
impl Drop for RequestMetrics {
    fn drop(&mut self) {
        if !self.succeeded {
            self.active.add(Event::RequestError, 1);
            self.active.add(request_error_reason(self.failure), 1);
            log_request_failure(self.failure);
        }
    }
}

/// The reason counter for a failed client request.
fn request_error_reason(failure: Option<crate::error::Error>) -> Event {
    use crate::error::Error;
    match failure {
        Some(Error::Overloaded | Error::FlightFull) => Event::RequestErrorOverloaded,
        Some(Error::Unavailable) => Event::RequestErrorUnavailable,
        Some(Error::DeadlineExceeded) => Event::RequestErrorDeadline,
        Some(Error::Cancelled) => Event::RequestErrorCancelled,
        _ => Event::RequestErrorOther,
    }
}

/// Log failed client requests at most once per [`FAILURE_LOG_INTERVAL`] per
/// thread, with how many were suppressed in between, so a client whose
/// stream ends early can find out why without flooding the log.
fn log_request_failure(failure: Option<crate::error::Error>) {
    use std::cell::Cell;
    thread_local! {
        static NEXT: Cell<Option<std::time::Instant>> = const { Cell::new(None) };
        static SUPPRESSED: Cell<u64> = const { Cell::new(0) };
    }
    let now = std::time::Instant::now();
    if NEXT.get().is_some_and(|next| now < next) {
        SUPPRESSED.set(SUPPRESSED.get().saturating_add(1));
        return;
    }
    NEXT.set(Some(now + FAILURE_LOG_INTERVAL));
    let suppressed = SUPPRESSED.replace(0);
    match failure {
        Some(error) => eprintln!(
            "racer: stage=client-request state=failed error={error:?} suppressed={suppressed}; see /debug/terminal"
        ),
        None => eprintln!(
            "racer: stage=client-request state=failed error=unreported suppressed={suppressed}; see /debug/terminal"
        ),
    }
}

/// Least time between client-request failure log lines on one thread.
const FAILURE_LOG_INTERVAL: Duration = Duration::from_secs(10);
impl Metrics {
    pub(crate) fn observe_held(&self, snapshot: crate::store::catalog::HeldSnapshot) {
        self.held.replace(self.shard, snapshot);
    }

    fn write_held(&self, out: &mut impl Write) -> std::fmt::Result {
        let mut totals = [0u64; 3];
        for snapshot in self.held.snapshots() {
            for (total, value) in
                totals
                    .iter_mut()
                    .zip([snapshot.pages, snapshot.payload_bytes, snapshot.segments])
            {
                *total = total.saturating_add(value);
            }
        }
        crate::telemetry::primitives::metrics::Exposition::new(HELD_METRICS).write(out, totals)
    }

    /// Node-wide prefetch counters.
    pub(crate) fn prefetch(&self) -> &Arc<crate::prefetch::metrics::PrefetchMetrics> {
        &self.prefetch
    }

    pub(crate) fn observe_disks(&self, samples: Vec<DiskUsage>) {
        self.disks.replace(self.shard, samples);
    }

    fn write_disks(&self, out: &mut impl Write) -> std::fmt::Result {
        let mut totals = std::collections::BTreeMap::<Arc<str>, (u64, u64)>::new();
        for shard in self.disks.snapshots() {
            for sample in shard {
                let total = totals.entry(sample.disk).or_default();
                total.0 = total.0.saturating_add(sample.size);
                total.1 = total.1.saturating_add(sample.used);
            }
        }
        if !totals.is_empty() {
            for metric in ["racer_disk_size_bytes", "racer_disk_used_bytes"] {
                writeln!(out, "# TYPE {metric} gauge")?;
            }
        }
        for (disk, (size, used)) in totals {
            for (metric, value) in [
                ("racer_disk_size_bytes", size),
                ("racer_disk_used_bytes", used),
            ] {
                write!(out, "{metric}{{disk=\"")?;
                write_disk_label(out, &disk)?;
                writeln!(out, "\"}} {value}")?;
            }
        }
        Ok(())
    }

    /// Replace this worker's per-cache disk samples. Only published caches are
    /// sampled, so a removed cache's series disappear on the next tick.
    pub(crate) fn observe_cache_disks(&self, samples: Vec<CacheDiskUsage>) {
        self.cache_disks.replace(self.shard, samples);
    }

    /// Per-cache disk totals across workers, sorted by cache name.
    fn cache_disk_totals(&self) -> Vec<(Arc<str>, u64, u64)> {
        let mut totals = std::collections::BTreeMap::<Arc<str>, (u64, u64)>::new();
        for shard in self.cache_disks.snapshots() {
            for sample in shard {
                let total = totals.entry(sample.cache).or_default();
                total.0 = total.0.saturating_add(sample.resident);
                total.1 = total.1.saturating_add(sample.share);
            }
        }
        totals
            .into_iter()
            .map(|(cache, (resident, share))| (cache, resident, share))
            .collect()
    }

    #[cfg(test)]
    fn write_cache_disks(&self, out: &mut impl Write) -> std::fmt::Result {
        let totals = self.cache_disk_totals();
        if !totals.is_empty() {
            for metric in CACHE_DISK_METRICS {
                writeln!(out, "# TYPE {metric} gauge")?;
            }
        }
        totals
            .iter()
            .try_for_each(|item| Self::write_cache_disk(out, item))
    }

    fn write_cache_disk(
        out: &mut impl Write,
        (cache, resident, share): &(Arc<str>, u64, u64),
    ) -> std::fmt::Result {
        for (metric, value) in CACHE_DISK_METRICS.into_iter().zip([*resident, *share]) {
            write!(out, "{metric}{{cache=\"")?;
            write_disk_label(out, cache)?;
            writeln!(out, "\"}} {value}")?;
        }
        Ok(())
    }

    /// Sample on the owning worker's health tick, never on a request hot path.
    /// Diagnostics aggregate fixed-size copies without accessing worker-local Rc.
    pub(crate) fn observe_retention(&self, snapshot: crate::retention::Snapshot) {
        self.retention.replace(self.shard, snapshot);
    }

    fn write_retention(&self, out: &mut impl Write) -> std::fmt::Result {
        let mut totals = [0u64; RETENTION_METRICS.len()];
        let mut classes = [[0u64; DISK_CLASS_METRICS.len()]; 2];
        for snapshot in self.retention.snapshots() {
            for (total, value) in totals.iter_mut().zip([
                snapshot.observations,
                snapshot.qualified,
                snapshot.persistence_attempts,
                snapshot.persistence_accepted,
                snapshot.filter_set_bits as u64,
                snapshot.filter_bits as u64,
                snapshot.pending_payload_bytes,
                snapshot.indexed_payload_bytes,
                snapshot.share_rejected,
            ]) {
                *total = total.saturating_add(value);
            }
            for (totals, class) in classes.iter_mut().zip(snapshot.disk) {
                for (total, value) in totals.iter_mut().zip([
                    class.published_pages,
                    class.published_payload_bytes,
                    class.segment_evicted_pages,
                    class.segment_evicted_payload_bytes,
                    class.read_payload_bytes,
                ]) {
                    *total = total.saturating_add(value);
                }
            }
        }
        crate::telemetry::primitives::metrics::Exposition::new(RETENTION_METRICS).write(out, totals)?;
        crate::telemetry::primitives::metrics::Exposition::new(DISK_CLASS_METRICS.map(|name| (name, "counter")))
            .write_labeled(out, "classification", ["nonowned", "owned"], classes)
    }

    pub(crate) fn opaque_relay_body(&self, bytes: usize) -> Option<OpaqueRelayBody<'_>> {
        (bytes != 0).then(|| OpaqueRelayBody {
            metrics: self,
            bytes,
            completed: false,
        })
    }
    /// Replace this worker's slab staging pool statistics from its health tick.
    pub(crate) fn observe_staging_pool(&self, worker: WorkerId, stats: flow_control::PoolStats) {
        self.memory
            .staging
            .replace(self.shard, Some((worker, stats)));
    }

    /// Replace the node-wide process and cgroup memory sample.
    pub(crate) fn observe_memory(&self, memory: ProcessMemory) {
        self.memory.process.replace(0, memory);
    }

    fn write_memory(&self, out: &mut impl Write) -> std::fmt::Result {
        let Some(memory) = self.memory.process.snapshots().next() else {
            return Ok(());
        };
        for (name, value) in [
            ("racer_process_resident_bytes", memory.resident),
            ("racer_cgroup_memory_current_bytes", memory.cgroup_current),
            ("racer_cgroup_memory_limit_bytes", memory.cgroup_limit),
        ] {
            if let Some(value) = value {
                writeln!(out, "# TYPE {name} gauge\n{name} {value}")?;
            }
        }
        if let Some(avg10) = memory.pressure_some_avg10.filter(|v| v.is_finite()) {
            let name = "racer_cgroup_memory_pressure_some_avg10";
            writeln!(out, "# TYPE {name} gauge\n{name} {avg10}")?;
        }
        Ok(())
    }

    /// Write node-wide admission for every class, plus staging pools.
    fn write_worker_pools(&self, out: &mut impl Write) -> std::fmt::Result {
        let installed: Vec<_> = self
            .admission
            .iter()
            .filter_map(|shard| shard.get())
            .collect();
        if !installed.is_empty() {
            for (name, used) in [
                ("racer_admission_used", true),
                ("racer_admission_limit", false),
            ] {
                writeln!(out, "# TYPE {name} gauge")?;
                for (class, label) in ADMISSION_CLASSES {
                    let total = installed.iter().fold(0u64, |total, (_, usage)| {
                        let value = if used {
                            usage.used(class)
                        } else {
                            usage.limit(class)
                        };
                        total.saturating_add(value as u64)
                    });
                    writeln!(out, "{name}{{class=\"{label}\"}} {total}")?;
                }
            }
        }
        let staging: Vec<_> = self.memory.staging.snapshots().flatten().collect();
        write_pools(out, "staging_pool", &staging)
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
        self.record(events[outcome], 1);
        result
    }
    /// Allocate a fixed registry at startup, with one event writer per worker.
    /// No registration, locking, or registry reference-count changes on record.
    pub(crate) fn for_workers(count: usize) -> Result<Vec<Self>> {
        if count == 0 {
            return Err(crate::error::Error::InvalidConfiguration);
        }
        let admission: Arc<[_]> = (0..count).map(|_| OnceLock::new()).collect();
        let retention = crate::telemetry::primitives::SnapshotShards::new(count);
        let held = crate::telemetry::primitives::SnapshotShards::new(count);
        let disks = crate::telemetry::primitives::SnapshotShards::new(count);
        let memory = Arc::new(MemoryShards {
            staging: crate::telemetry::primitives::SnapshotShards::new(count),
            process: crate::telemetry::primitives::SnapshotShards::new(1),
        });
        let cache_disks = crate::telemetry::primitives::SnapshotShards::new(count);
        let gpu = Arc::new(crate::gpu_telemetry::GpuTelemetry::default());
        let prefetch = crate::prefetch::metrics::PrefetchMetrics::shards(count);
        Ok(crate::telemetry::primitives::Metrics::shards(count)
            .into_iter()
            .zip(prefetch)
            .enumerate()
            .map(|(shard, (core, prefetch))| Self {
                core,
                admission: admission.clone(),
                retention: retention.clone(),
                held: held.clone(),
                disks: disks.clone(),
                memory: memory.clone(),
                cache_disks: cache_disks.clone(),
                gpu: gpu.clone(),
                prefetch: Arc::new(prefetch),
                shard,
            })
            .collect())
    }

    pub(crate) fn request(&self) -> Result<RequestMetrics> {
        let active = self
            .core
            .lease_local(Gauge::ActiveRequests)
            .ok_or(crate::error::Error::Overloaded)?;
        active.add(Event::Request, 1);
        Ok(RequestMetrics {
            active,
            succeeded: false,
            overloaded: false,
            failure: None,
        })
    }
    /// Add with wrapping arithmetic; Prometheus reads a wrap as a counter reset.
    pub fn record(&self, event: Event, amount: u64) {
        self.core.add(event, amount);
    }
    /// Add one observation of the time from `start` to `now` to a count and
    /// nanosecond sum pair.
    pub fn record_duration(
        &self,
        count: Event,
        sum: Event,
        start: std::time::Instant,
        now: std::time::Instant,
    ) {
        let elapsed = now.saturating_duration_since(start);
        self.record(sum, u64::try_from(elapsed.as_nanos()).unwrap_or(u64::MAX));
        self.record(count, 1);
    }
    /// GPU metric sources shared by every worker.
    /// Count one `gpu-read` request head on this worker's shard.
    pub(crate) fn record_gpu_request(&self, outcome: crate::gpu_telemetry::GpuReadOutcome) {
        self.record(gpu_read_event(outcome), 1);
    }
    /// `gpu-read` request heads counted with `outcome` across workers.
    #[cfg(test)]
    pub(crate) fn gpu_requests(&self, outcome: crate::gpu_telemetry::GpuReadOutcome) -> u64 {
        self.count(gpu_read_event(outcome))
    }
    pub fn gpu(&self) -> &crate::gpu_telemetry::GpuTelemetry {
        &self.gpu
    }
    /// A shared handle to the GPU metric sources, for the lazy GPU start.
    pub fn gpu_shared(&self) -> Arc<crate::gpu_telemetry::GpuTelemetry> {
        self.gpu.clone()
    }
    pub(crate) fn writer_shard(&self) -> usize {
        self.shard
    }
    #[cfg(test)]
    pub(crate) fn writer_count(&self, event: Event) -> u64 {
        self.core.count_on(self.shard, event)
    }
    pub(crate) fn record_on(&self, shard: usize, event: Event, amount: u64) {
        self.core.add_on(shard, event, amount);
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
    /// Keep with the actual resource, including through a submitted I/O fence.
    pub fn lease(&self, gauge: Gauge) -> Result<crate::telemetry::primitives::Lease> {
        self.core
            .lease(gauge)
            .ok_or(crate::error::Error::Overloaded)
    }
    /// The destination is bounded by the diagnostic server; no intermediate String.
    /// Relaxed per-series observations are not a coherent snapshot of all workers.
    pub fn write_prometheus(&self, out: &mut impl std::fmt::Write) -> std::fmt::Result {
        let mut exposition = self.exposition();
        while exposition.next(out)? {}
        Ok(())
    }

    /// A resumable exposition written one bounded piece at a time, so the
    /// diagnostic server can stream it without a fixed body cap. Pieces are
    /// fixed sections, except per-cache series, which are one piece per cache.
    pub(crate) fn exposition(&self) -> Exposition<'_> {
        Exposition {
            metrics: self,
            stage: 0,
            caches: None,
        }
    }

    fn write_quotas(&self, out: &mut impl std::fmt::Write) -> std::fmt::Result {
        // Quota labels use runtime worker IDs. Read the authority's actual charge,
        // including pooled ciphertext capacity, without sampling on worker polls.
        // A stalled worker therefore remains observable from another worker.
        const QUOTAS: crate::telemetry::primitives::metrics::LabeledGauges<4> =
            crate::telemetry::primitives::metrics::LabeledGauges::new(
                [
                    "racer_worker_relay_used",
                    "racer_worker_relay_limit",
                    "racer_worker_ciphertext_used_bytes",
                    "racer_worker_ciphertext_limit_bytes",
                ],
                "worker",
            );
        if self.admission.iter().any(|s| s.get().is_some()) {
            QUOTAS.write_types(out)?;
            for shard in self.admission.iter() {
                if let Some((worker, usage)) = shard.get() {
                    let (relay_used, relay_limit) = (
                        usage.used(crate::admission::ResourceClass::Relay),
                        usage.limit(crate::admission::ResourceClass::Relay),
                    );
                    let (ciphertext_used, ciphertext_limit) = (
                        usage.used(crate::admission::ResourceClass::Ciphertext),
                        usage.limit(crate::admission::ResourceClass::Ciphertext),
                    );
                    QUOTAS.write_sample(
                        out,
                        u64::from(worker.0),
                        [relay_used, relay_limit, ciphertext_used, ciphertext_limit]
                            .map(|value| value as u64),
                    )?;
                }
            }
        }
        Ok(())
    }
}

/// Resumable `/metrics` writer. A failed piece leaves the state unchanged,
/// so the caller can retry it with fresh space.
pub(crate) struct Exposition<'a> {
    metrics: &'a Metrics,
    stage: usize,
    /// Per-cache totals, captured once when the cache section starts.
    caches: Option<Vec<(Arc<str>, u64, u64)>>,
}

impl Exposition<'_> {
    /// Fixed sections before the per-cache series.
    const HEAD: usize = 6;
    /// Fixed sections after the per-cache series.
    const TAIL: usize = 3;

    /// Write the next piece; `Ok(true)` while more remain.
    pub(crate) fn next(
        &mut self,
        out: &mut impl Write,
    ) -> std::result::Result<bool, std::fmt::Error> {
        let metrics = self.metrics;
        match self.stage {
            0 => metrics.core.write_prometheus(out)?,
            1 => metrics.write_retention(out)?,
            2 => metrics.write_held(out)?,
            3 => metrics.write_disks(out)?,
            4 => metrics.write_memory(out)?,
            5 => metrics.write_worker_pools(out)?,
            stage => {
                let caches = self
                    .caches
                    .get_or_insert_with(|| metrics.cache_disk_totals());
                // Stage HEAD writes the type lines, then one stage per cache.
                let cache = stage - Self::HEAD;
                if cache == 0 {
                    if !caches.is_empty() {
                        for metric in CACHE_DISK_METRICS {
                            writeln!(out, "# TYPE {metric} gauge")?;
                        }
                    }
                } else if let Some(item) = caches.get(cache - 1) {
                    Metrics::write_cache_disk(out, item)?;
                } else {
                    match cache - 1 - caches.len() {
                        0 => metrics.gpu.write(out)?,
                        1 => metrics.prefetch.write(out)?,
                        _ => metrics.write_quotas(out)?,
                    }
                }
                self.stage += 1;
                return Ok(stage - Self::HEAD < caches.len() + Self::TAIL);
            }
        }
        self.stage += 1;
        Ok(true)
    }
}

#[cfg(test)]
mod held_metric_tests {
    use super::*;

    #[test]
    fn held_worker_snapshots_replace_aggregate_saturate_and_survive_failed_output() {
        use crate::store::catalog::HeldSnapshot;
        let workers = Metrics::for_workers(2).unwrap();
        let snapshot = HeldSnapshot {
            pages: 2,
            payload_bytes: 17,
            segments: 1,
        };
        workers[0].observe_held(snapshot);
        workers[0].observe_held(snapshot);
        workers[1].observe_held(snapshot);
        let mut output = String::new();
        workers[1].write_prometheus(&mut output).unwrap();
        for (name, value) in HELD_METRICS.into_iter().zip([4, 34, 2]) {
            assert!(output.contains(&format!("# TYPE {} gauge\n{} {value}\n", name.0, name.0)));
        }
        workers[0].observe_held(HeldSnapshot::default());
        output.clear();
        workers[0].write_held(&mut output).unwrap();
        assert!(output.contains("racer_held_payload_bytes 17\n"));
        for worker in &workers {
            worker.observe_held(HeldSnapshot {
                pages: u64::MAX,
                payload_bytes: u64::MAX,
                segments: u64::MAX,
            });
        }
        struct Full;
        impl Write for Full {
            fn write_str(&mut self, _: &str) -> std::fmt::Result {
                Err(std::fmt::Error)
            }
        }
        assert!(workers[1].write_held(&mut Full).is_err());
        output.clear();
        workers[0].write_held(&mut output).unwrap();
        for (name, _) in HELD_METRICS {
            assert!(output.contains(&format!("{name} {}\n", u64::MAX)));
        }
    }

    #[test]
    fn prefetch_metrics_are_exported_with_priority_labels_only() {
        let telemetry = Telemetry::default();
        telemetry.metrics.prefetch().add(
            crate::prefetch::metrics::Counter::RequestedBytes,
            crate::prefetch::Priority::High,
            5,
        );
        let mut output = String::new();
        assert!(matches!(
            get(&telemetry, "/metrics", true, &mut output),
            Ok(Response::Metrics)
        ));
        assert!(output.contains("racer_prefetch_requested_bytes_total{priority=\"high\"} 5\n"));
        assert!(!output.contains("racer_warm_"));
        assert!(output.len() < MAX_RESPONSE_BYTES);
    }
}

#[cfg(test)]
mod retention_metric_tests {
    use super::*;

    #[test]
    fn disk_usage_aggregates_shared_disks_replaces_and_escapes_labels() {
        let workers = Metrics::for_workers(2).unwrap();
        let mut output = String::new();
        workers[0].write_disks(&mut output).unwrap();
        assert!(output.is_empty());
        let sample = |disk: &str, size, used| DiskUsage {
            disk: disk.into(),
            size,
            used,
        };
        workers[0].observe_disks(vec![
            sample("device:a", 1024, 512),
            sample("file:worker-0-slab-0.dat", 4096, 0),
        ]);
        workers[1].observe_disks(vec![
            sample("device:a", 2048, 1024),
            sample("device:b\\\"\n", 512, 512),
        ]);
        workers[0].write_disks(&mut output).unwrap();
        assert!(output.contains("# TYPE racer_disk_size_bytes gauge\n"));
        assert!(output.contains("# TYPE racer_disk_used_bytes gauge\n"));
        assert!(output.contains("racer_disk_size_bytes{disk=\"device:a\"} 3072\n"));
        assert!(output.contains("racer_disk_used_bytes{disk=\"device:a\"} 1536\n"));
        assert!(output.contains("racer_disk_used_bytes{disk=\"device:b\\\\\\\"\\n\"} 512\n"));
        assert!(output.contains("racer_disk_used_bytes{disk=\"file:worker-0-slab-0.dat\"} 0\n"));
        workers[1].observe_disks(vec![sample("device:a", 2048, 0)]);
        workers[1].observe_disks(vec![sample("device:a", 2048, 0)]);
        output.clear();
        workers[0].write_disks(&mut output).unwrap();
        assert!(output.contains("racer_disk_size_bytes{disk=\"device:a\"} 3072\n"));
        assert!(output.contains("racer_disk_used_bytes{disk=\"device:a\"} 512\n"));
        assert!(!output.contains("device:b"));
        workers[0].observe_disks(vec![]);
        workers[1].observe_disks(vec![]);
        output.clear();
        workers[0].write_disks(&mut output).unwrap();
        assert!(output.is_empty());
    }

    #[test]
    fn disk_usage_export_saturates_and_keeps_samples_on_output_failure() {
        struct Full;
        impl Write for Full {
            fn write_str(&mut self, _: &str) -> std::fmt::Result {
                Err(std::fmt::Error)
            }
        }
        let workers = Metrics::for_workers(2).unwrap();
        for worker in &workers {
            worker.observe_disks(vec![DiskUsage {
                disk: "device:a".into(),
                size: u64::MAX,
                used: u64::MAX,
            }]);
        }
        assert!(workers[0].write_disks(&mut Full).is_err());
        let mut output = String::new();
        workers[1].write_disks(&mut output).unwrap();
        for name in ["size", "used"] {
            assert!(output.contains(&format!(
                "racer_disk_{name}_bytes{{disk=\"device:a\"}} {}\n",
                u64::MAX
            )));
        }
    }

    #[test]
    fn cache_disk_usage_sums_workers_by_name_and_drops_removed_caches() {
        let workers = Metrics::for_workers(2).unwrap();
        let mut output = String::new();
        workers[0].write_cache_disks(&mut output).unwrap();
        assert!(output.is_empty());
        let sample = |cache: &str, resident, share| CacheDiskUsage {
            cache: cache.into(),
            resident,
            share,
        };
        workers[0].observe_cache_disks(vec![sample("models", 900, 800), sample("logs", 50, 200)]);
        workers[1].observe_cache_disks(vec![sample("models", 100, 800), sample("x\"\n", 1, 2)]);
        workers[1].write_prometheus(&mut output).unwrap();
        assert!(output.contains("# TYPE racer_cache_disk_resident_bytes gauge\n"));
        assert!(output.contains("# TYPE racer_cache_disk_share_bytes gauge\n"));
        assert!(output.contains("racer_cache_disk_resident_bytes{cache=\"models\"} 1000\n"));
        assert!(output.contains("racer_cache_disk_share_bytes{cache=\"models\"} 1600\n"));
        assert!(output.contains("racer_cache_disk_resident_bytes{cache=\"logs\"} 50\n"));
        assert!(output.contains("racer_cache_disk_share_bytes{cache=\"x\\\"\\n\"} 2\n"));
        workers[0].observe_cache_disks(vec![sample("models", 0, 1000)]);
        workers[1].observe_cache_disks(vec![]);
        output.clear();
        workers[0].write_cache_disks(&mut output).unwrap();
        assert!(!output.contains("logs"));
        assert!(output.contains("racer_cache_disk_share_bytes{cache=\"models\"} 1000\n"));
    }

    #[test]
    fn disk_observability_aggregates_fixed_classes_and_replaces_gauges() {
        let workers = Metrics::for_workers(2).unwrap();
        for (i, worker) in workers.iter().enumerate() {
            let mut snapshot = crate::retention::Snapshot {
                pending_payload_bytes: 3,
                indexed_payload_bytes: 17,
                ..Default::default()
            };
            snapshot.disk[i] = crate::retention::DiskClassSnapshot {
                published_pages: 1,
                published_payload_bytes: 17,
                segment_evicted_pages: 3,
                segment_evicted_payload_bytes: 51,
                read_payload_bytes: u64::MAX,
            };
            worker.observe_retention(snapshot);
            worker.observe_retention(snapshot);
        }
        let mut output = String::new();
        workers[0].write_retention(&mut output).unwrap();
        for class in ["nonowned", "owned"] {
            for (name, value) in DISK_CLASS_METRICS.into_iter().zip([1, 17, 3, 51, u64::MAX]) {
                assert!(output.contains(&format!("# TYPE {name} counter\n")));
                assert!(
                    output.contains(&format!("{name}{{classification=\"{class}\"}} {value}\n"))
                );
            }
        }
        assert_eq!(
            output
                .lines()
                .filter(|line| line.contains("{classification="))
                .count(),
            10
        );
        assert!(output.contains("racer_disk_pending_payload_bytes 6\n"));
        assert!(output.contains("racer_disk_indexed_payload_bytes 34\n"));
        workers[0].observe_retention(crate::retention::Snapshot::default());
        output.clear();
        workers[1].write_retention(&mut output).unwrap();
        assert!(output.contains("racer_disk_pending_payload_bytes 3\n"));
        assert!(output.contains("racer_disk_indexed_payload_bytes 17\n"));
    }
    #[test]
    fn retention_snapshots_replace_shards_and_aggregate_without_labels() {
        let workers = Metrics::for_workers(2).unwrap();
        workers[0].observe_retention(crate::retention::Snapshot {
            observations: 10,
            qualified: 4,
            persistence_attempts: 3,
            persistence_accepted: 2,
            filter_set_bits: 7,
            filter_bits: 256,
            ..Default::default()
        });
        workers[1].observe_retention(crate::retention::Snapshot {
            observations: 5,
            filter_set_bits: 2,
            filter_bits: 256,
            ..Default::default()
        });
        // Repeated health observations must not add cumulative counters again.
        let snapshot = workers[0].retention.snapshots().next().unwrap();
        workers[0].observe_retention(snapshot);
        let mut output = String::new();
        workers[1].write_prometheus(&mut output).unwrap();
        for sample in [
            "racer_retention_observations_total 15\n",
            "racer_retention_qualified_total 4\n",
            "racer_retention_persistence_attempts_total 3\n",
            "racer_retention_persistence_accepted_total 2\n",
            "racer_retention_filter_set_bits 9\n",
            "racer_retention_filter_bits 512\n",
        ] {
            assert!(output.contains(sample), "{sample}");
        }
        assert!(
            !output
                .lines()
                .any(|line| line.starts_with("racer_retention_") && line.contains('{'))
        );
        assert!(output.contains("# TYPE racer_retention_observations_total counter\n"));
        assert!(output.contains("# TYPE racer_retention_filter_bits gauge\n"));
    }

    #[test]
    fn retention_export_saturates_and_propagates_output_failure() {
        let workers = Metrics::for_workers(2).unwrap();
        for worker in &workers {
            worker.observe_retention(crate::retention::Snapshot {
                observations: u64::MAX,
                disk: [crate::retention::DiskClassSnapshot {
                    read_payload_bytes: u64::MAX,
                    ..Default::default()
                }; 2],
                ..Default::default()
            });
        }
        let mut output = String::new();
        workers[0].write_retention(&mut output).unwrap();
        assert!(output.contains(&format!(
            "racer_retention_observations_total {}\n",
            u64::MAX
        )));
        for class in ["owned", "nonowned"] {
            assert!(output.contains(&format!(
                "racer_disk_class_read_payload_bytes_total{{classification=\"{class}\"}} {}\n",
                u64::MAX
            )));
        }
        struct Failed;
        impl Write for Failed {
            fn write_str(&mut self, _: &str) -> std::fmt::Result {
                Err(std::fmt::Error)
            }
        }
        assert!(workers[0].write_retention(&mut Failed).is_err());
    }

    #[test]
    fn retention_metrics_endpoint_is_nonconsuming_when_admission_stops() {
        let telemetry = Telemetry::default();
        telemetry
            .metrics
            .observe_retention(crate::retention::Snapshot {
                observations: 17,
                qualified: 5,
                ..Default::default()
            });
        for usable in [true, false, true] {
            let mut output = String::new();
            assert!(matches!(
                get(&telemetry, "/metrics", usable, &mut output),
                Ok(Response::Metrics)
            ));
            assert!(output.contains("racer_retention_observations_total 17\n"));
            assert!(output.contains("racer_retention_qualified_total 5\n"));
            assert!(output.len() < MAX_RESPONSE_BYTES);
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    pub(crate) mod metrics_tests {
        use super::*;
        use crate::admission::ResourceClass;

        #[test]
        fn request_lease_overflow_preserves_counters_and_error() {
            let metrics = Metrics::default();
            metrics.core.set_local(Gauge::ActiveRequests, u64::MAX);
            assert!(matches!(
                metrics.request(),
                Err(crate::error::Error::Overloaded)
            ));
            assert_eq!(metrics.count(Event::Request), 0);
            assert_eq!(metrics.count(Event::RequestError), 0);
            assert_eq!(metrics.gauge(Gauge::ActiveRequests), u64::MAX);
        }

        #[test]
        fn failed_requests_count_their_first_reason() {
            use crate::error::Error;
            let metrics = Metrics::default();
            let mut overloaded = metrics.request().unwrap();
            overloaded.fail(Error::Overloaded);
            overloaded.fail(Error::Unavailable);
            drop(overloaded);
            let mut unavailable = metrics.request().unwrap();
            unavailable.fail(Error::Unavailable);
            drop(unavailable);
            drop(metrics.request().unwrap());
            let mut succeeded = metrics.request().unwrap();
            succeeded.fail(Error::DeadlineExceeded);
            succeeded.success();
            drop(succeeded);
            assert_eq!(metrics.count(Event::RequestError), 3);
            assert_eq!(metrics.count(Event::RequestErrorOverloaded), 1);
            assert_eq!(metrics.count(Event::RequestErrorUnavailable), 1);
            assert_eq!(metrics.count(Event::RequestErrorOther), 1);
            assert_eq!(metrics.count(Event::RequestErrorDeadline), 0);
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
        fn integrity_diagnostics_have_fixed_names_and_wrapping_sharded_counts() {
            let workers = Metrics::for_workers(2).unwrap();
            let events = [
                Event::CryptoDecryptAeadRejected,
                Event::FillDecryptDiskCorrupt,
                Event::FillDecryptRetainedCorrupt,
                Event::FillDecryptPeerCorrupt,
            ];
            for event in events {
                workers[0].record(event, u64::MAX);
                workers[0].record(event, 1);
                workers[1].record(event, 1);
                assert_eq!(workers[1].count(event), 1);
            }
            let mut output = String::new();
            workers[1].write_prometheus(&mut output).unwrap();
            for event in events {
                assert!(output.contains(&format!(
                    "# TYPE {} counter\n{} {}\n",
                    event.name(),
                    event.name(),
                    1
                )));
            }
            assert_only_disk_class_labels(&output);
            assert_eq!(
                EVENTS
                    .iter()
                    .map(|e| (e.name(), crate::telemetry::primitives::Metric::labels(*e)))
                    .collect::<std::collections::BTreeSet<_>>()
                    .len(),
                EVENT_COUNT
            );
        }

        #[test]
        fn every_metric_family_has_exactly_one_type_line() {
            let mut output = String::new();
            Metrics::default().write_prometheus(&mut output).unwrap();
            let mut seen = std::collections::BTreeSet::new();
            for line in output.lines().filter(|l| l.starts_with("# TYPE ")) {
                let name = line.split(' ').nth(2).unwrap();
                assert!(seen.insert(name.to_owned()), "duplicate {line}");
            }
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
                .observe_admission(WorkerId(7), admission.shared())
                .unwrap();
            workers[1]
                .observe_admission(WorkerId(19), other.shared())
                .unwrap();
            assert!(matches!(
                workers[0].observe_admission(WorkerId(20), other.shared()),
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
                    .filter(|line| line.starts_with("racer_worker_relay_")
                        || line.starts_with("racer_worker_ciphertext_"))
                    .count(),
                8
            );
            // Four compatibility series per worker.
            assert_eq!(
                idle.lines()
                    .filter(|line| line.contains("{worker="))
                    .count(),
                2 * 4
            );
            assert!(!idle.contains("request="));
        }

        #[test]
        fn worker_quota_gauges_include_live_ciphertext_charges() {
            let metrics = Metrics::default();
            let admission = flow_control::Quotas::new(AdmissionPolicy::new(
                crate::test_support::cluster::config(false).limits,
            ));
            metrics
                .observe_admission(WorkerId(0), admission.shared())
                .unwrap();
            let reservation = admission
                .reserve(None, ResourceClass::Ciphertext, 1 << 20)
                .unwrap();
            let mut output = String::new();
            metrics.write_prometheus(&mut output).unwrap();
            assert!(output.contains("racer_worker_ciphertext_used_bytes{worker=\"0\"} 1048576\n"));
            drop(reservation);
            output.clear();
            metrics.write_prometheus(&mut output).unwrap();
            assert!(output.contains("racer_worker_ciphertext_used_bytes{worker=\"0\"} 0\n"));
        }

        #[test]
        fn admission_classes_cover_every_class_in_index_order() {
            use flow_control::Class;
            assert_eq!(ADMISSION_CLASSES.len(), ResourceClass::COUNT);
            for (index, (class, label)) in ADMISSION_CLASSES.iter().enumerate() {
                assert_eq!(class.index(), index);
                assert!(label.chars().all(|c| c.is_ascii_lowercase() || c == '_'));
            }
        }

        #[test]
        fn admission_and_pool_series_sum_workers_and_label_retained_bytes() {
            let metrics = Metrics::for_workers(2).unwrap();
            let mut output = String::new();
            metrics[0].write_prometheus(&mut output).unwrap();
            // Nothing installed: no admission, pool, or process memory series.
            for absent in [
                "racer_admission_used",
                "racer_staging_pool_entries",
                "racer_process_resident_bytes",
            ] {
                assert!(!output.contains(absent), "{absent}");
            }
            let quotas: Vec<_> = (0..2)
                .map(|_| {
                    flow_control::Quotas::new(AdmissionPolicy::new(
                        crate::test_support::cluster::config(false).limits,
                    ))
                })
                .collect();
            for (index, (metrics, admission)) in metrics.iter().zip(&quotas).enumerate() {
                metrics
                    .observe_admission(WorkerId(3 + index as u16), admission.shared())
                    .unwrap();
            }
            let _ciphertext = quotas[0]
                .reserve(None, ResourceClass::Ciphertext, 1 << 20)
                .unwrap();
            let _relay = quotas[1].reserve(None, ResourceClass::Relay, 1).unwrap();
            let staging = |entries, bytes, counter| flow_control::PoolStats {
                entries,
                bytes,
                target: 3,
                max_depth: 8,
                hits: counter,
                misses: counter + 1,
                rejects: counter + 2,
                trimmed: counter + 3,
                constrained: false,
            };
            metrics[0].observe_staging_pool(WorkerId(3), staging(2, 4096, 5));
            metrics[1].observe_staging_pool(WorkerId(4), staging(1, 1024, 10));
            output.clear();
            metrics[1].write_prometheus(&mut output).unwrap();
            let sum = |class| quotas.iter().map(|q| q.limit(class)).sum::<usize>();
            let ciphertext_used = quotas[0].used(ResourceClass::Ciphertext);
            for expected in [
                "# TYPE racer_admission_used gauge\n".to_string(),
                "# TYPE racer_admission_limit gauge\n".to_string(),
                format!("racer_admission_used{{class=\"ciphertext\"}} {ciphertext_used}\n"),
                "racer_admission_used{class=\"relay\"} 1\n".to_string(),
                "racer_admission_used{class=\"plaintext\"} 0\n".to_string(),
                format!(
                    "racer_admission_limit{{class=\"relay\"}} {}\n",
                    sum(ResourceClass::Relay)
                ),
                format!(
                    "racer_admission_limit{{class=\"plaintext\"}} {}\n",
                    sum(ResourceClass::Plaintext)
                ),
                "# TYPE racer_staging_pool_entries gauge\nracer_staging_pool_entries 3\n"
                    .to_string(),
                "racer_staging_pool_bytes 5120\n".to_string(),
                "racer_staging_pool_target 6\n".to_string(),
                "# TYPE racer_staging_pool_hits_total counter\nracer_staging_pool_hits_total 15\n"
                    .to_string(),
                "racer_staging_pool_misses_total 17\n".to_string(),
                "racer_staging_pool_rejects_total 19\n".to_string(),
                "racer_staging_pool_trimmed_total 21\n".to_string(),
                "# TYPE racer_worker_staging_pool_bytes gauge\n".to_string(),
                "racer_worker_staging_pool_bytes{worker=\"3\"} 4096\n".to_string(),
                "racer_worker_staging_pool_bytes{worker=\"4\"} 1024\n".to_string(),
                // Compatibility series remain per worker.
                "racer_worker_relay_used{worker=\"4\"} 1\n".to_string(),
            ] {
                assert!(
                    output.contains(&expected),
                    "missing {expected:?} in {output}"
                );
            }
            assert!(!output.contains("recycler"), "{output}");
            assert_eq!(
                output
                    .lines()
                    .filter(|l| l.starts_with("racer_admission_used{"))
                    .count(),
                <ResourceClass as flow_control::Class>::COUNT
            );
            // Each TYPE line appears exactly once.
            let types: Vec<_> = output
                .lines()
                .filter(|l| l.starts_with("# TYPE "))
                .collect();
            let unique: std::collections::BTreeSet<_> = types.iter().collect();
            assert_eq!(types.len(), unique.len());
        }

        #[test]
        fn process_memory_series_omit_absent_signals() {
            let metrics = Metrics::default();
            metrics.observe_memory(ProcessMemory {
                resident: Some(4096),
                ..Default::default()
            });
            let mut output = String::new();
            metrics.write_prometheus(&mut output).unwrap();
            assert!(output.contains(
                "# TYPE racer_process_resident_bytes gauge\nracer_process_resident_bytes 4096\n"
            ));
            assert!(!output.contains("racer_cgroup_memory"));
            metrics.observe_memory(ProcessMemory {
                resident: Some(1),
                cgroup_current: Some(800),
                cgroup_limit: Some(1000),
                pressure_some_avg10: Some(12.5),
            });
            metrics.set_gauge(Gauge::MemoryPressureLevel, 2);
            metrics.record(Event::MemoryPressureTransitions, 3);
            metrics.record(Event::AllocatorTrims, 1);
            output.clear();
            metrics.write_prometheus(&mut output).unwrap();
            for expected in [
                "racer_cgroup_memory_current_bytes 800\n",
                "racer_cgroup_memory_limit_bytes 1000\n",
                "racer_cgroup_memory_pressure_some_avg10 12.5\n",
                "# TYPE racer_memory_pressure_level gauge\nracer_memory_pressure_level 2\n",
                "# TYPE racer_memory_pressure_transitions_total counter\nracer_memory_pressure_transitions_total 3\n",
                "# TYPE racer_allocator_trims_total counter\nracer_allocator_trims_total 1\n",
            ] {
                assert!(output.contains(expected), "missing {expected:?}");
            }
            // Non-finite PSI is not exported.
            metrics.observe_memory(ProcessMemory {
                pressure_some_avg10: Some(f64::NAN),
                ..Default::default()
            });
            output.clear();
            metrics.write_prometheus(&mut output).unwrap();
            assert!(!output.contains("racer_cgroup_memory_pressure_some_avg10"));
            assert!(!output.contains("racer_process_resident_bytes"));
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
        fn fixed_series_wrap_and_leases_return_to_baseline() {
            let metrics = Metrics::default();
            for event in EVENTS {
                metrics.record(event, u64::MAX);
                metrics.record(event, 9);
                assert_eq!(metrics.count(event), 8);
            }
            let lease = metrics.lease(Gauge::ActiveRequests).unwrap();
            assert_eq!(metrics.clone().gauge(Gauge::ActiveRequests), 1);
            drop(lease);
            assert_eq!(metrics.gauge(Gauge::ActiveRequests), 0);
            let mut output = String::new();
            metrics.write_prometheus(&mut output).unwrap();
            assert_eq!(
                output.lines().filter(|line| !line.starts_with('#')).count(),
                EVENT_COUNT
                    + GAUGE_COUNT
                    + RETENTION_METRICS.len()
                    + HELD_METRICS.len()
                    + crate::prefetch::metrics::SAMPLE_LINES
                    + DISK_CLASS_METRICS.len() * 2
                    + crate::gpu_telemetry::BASE_SERIES
            );
            assert_only_disk_class_labels(&output);
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
                clone.record(Event::MemoryHit, (index + 1) as u64);
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
                            worker.record(Event::MemoryHit, 1);
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
                        EVENT_COUNT
                            + GAUGE_COUNT
                            + RETENTION_METRICS.len()
                            + HELD_METRICS.len()
                            + crate::prefetch::metrics::SAMPLE_LINES
                            + DISK_CLASS_METRICS.len() * 2
                            + crate::gpu_telemetry::BASE_SERIES
                    );
                    assert_only_disk_class_labels(&output);
                }
            });
            assert_eq!(reader.count(Event::Request), 4000);
            assert_eq!(reader.count(Event::MemoryHit), 4000);
            assert_eq!(reader.count(Event::RequestError), 0);
            assert_eq!(reader.gauge(Gauge::ActiveRequests), 0);
        }

        #[test]
        fn aggregate_events_wrap_like_a_counter_reset() {
            let workers = Metrics::for_workers(2).unwrap();
            for event in EVENTS {
                workers[0].record(event, u64::MAX - 1);
                workers[1].record(event, 2);
                assert_eq!(workers[0].count(event), 0);
                workers[1].record(event, u64::MAX);
                assert_eq!(workers[1].count(event), u64::MAX);
            }
        }

        fn assert_only_disk_class_labels(output: &str) {
            let labeled: Vec<_> = output
                .lines()
                .filter(|line| {
                    line.contains('{')
                        && !line.starts_with("racer_gpu_read_requests_total{")
                        && !line.starts_with("racer_prefetch_")
                })
                .collect();
            assert_eq!(labeled.len(), DISK_CLASS_METRICS.len() * 2);
            for line in labeled {
                let (name, rest) = line.split_once('{').unwrap();
                assert!(DISK_CLASS_METRICS.contains(&name));
                let (labels, _) = rest.split_once('}').unwrap();
                assert!(
                    matches!(
                        labels,
                        "classification=\"owned\"" | "classification=\"nonowned\""
                    ),
                    "no identity or content labels"
                );
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
}
