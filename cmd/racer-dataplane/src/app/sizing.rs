//! Partition node budgets and reduce worker placement to preserve progress floors.
use super::native;
use crate::{
    error::{Error, Result},
    model::Limits,
    runtime::{admission::Admission, affinity::AffinityPlan},
};
use std::num::NonZeroUsize;

pub(super) fn partition_limits_with_cause(
    node: &Limits,
    workers: usize,
    rdma: bool,
) -> std::result::Result<Limits, (&'static str, Error)> {
    let invalid = |dimension| (dimension, Error::InvalidConfiguration);
    if workers == 0 {
        return Err(invalid("io_workers"));
    }
    let mut limits = node.clone();
    for (dimension, value) in [
        ("plaintext_bytes", &mut limits.plaintext_bytes),
        ("ciphertext_bytes", &mut limits.ciphertext_bytes),
        ("dirty_bytes", &mut limits.dirty_bytes),
        ("request_context_bytes", &mut limits.request_context_bytes),
        ("flights", &mut limits.flights),
        ("queue_entries", &mut limits.queue_entries),
        ("client_connections", &mut limits.client_connections),
        ("pipes", &mut limits.pipes),
        ("cached_rankings", &mut limits.cached_rankings),
        ("cached_paths", &mut limits.cached_paths),
        ("metadata_entries", &mut limits.metadata_entries),
        ("relay_transfers", &mut limits.relay_transfers),
    ] {
        *value = NonZeroUsize::new(value.get() / workers).ok_or_else(|| invalid(dimension))?;
    }
    if rdma {
        limits.registered_bytes = NonZeroUsize::new(node.registered_bytes.get() / workers)
            .ok_or_else(|| invalid("registered_bytes"))?;
    }
    let page = crate::model::PAGE_BYTES as usize;
    let window = limits.range_window_pages.get();
    for (dimension, insufficient) in [
        (
            "plaintext_bytes",
            limits.plaintext_bytes.get() < (window + 1) * page,
        ),
        (
            "ciphertext_bytes",
            limits.ciphertext_bytes.get()
                < (window + 1) * (page + 16) + crate::store::format::MAX_HEADER_BYTES,
        ),
        ("dirty_bytes", limits.dirty_bytes.get() < page + 16),
        (
            "registered_bytes",
            rdma && native::slot_count(&limits).map_err(|error| ("registered_bytes", error))? == 0,
        ),
        (
            "request_context_bytes",
            limits.request_context_bytes.get()
                < crate::peer::protocol::MIN_REQUEST_CONTEXT_BYTES
                    + 4 * limits.header_bytes.get().max(crate::model::MAX_FIELD_BYTES),
        ),
        ("queue_entries", limits.queue_entries.get() < 2),
        (
            "client_connections",
            limits.client_connections < limits.connections_per_neighbor,
        ),
    ] {
        if insufficient {
            return Err(invalid(dimension));
        }
    }
    // Snapshot polling, renewal, and key delivery need independent control slots.
    // The same partition also funds outbound progress. Per-neighbor concurrency
    // is a ceiling, not a promise of that many shared outbound slots.
    let admission = Admission::new(limits.clone());
    if admission.limit(crate::model::ResourceClass::ControlConnection) < 3 {
        return Err(invalid("control_connections"));
    }
    Ok(limits)
}

pub(super) fn log_worker_plan(stage: &str, plan: &AffinityPlan) {
    let groups = plan.crypto_groups();
    eprintln!(
        "racer-dataplane: stage=worker-plan state={stage} io_workers={} crypto_workers={}",
        plan.pairs.len(),
        groups.len()
    );
    for indices in groups {
        let crypto = &plan.pairs[indices[0]].crypto;
        let io = indices
            .iter()
            .map(|&index| {
                let pair = &plan.pairs[index];
                (pair.worker.0, pair.io.cpu, pair.io.numa_node)
            })
            .collect::<Vec<_>>();
        eprintln!(
            "racer-dataplane: stage=worker-placement state={stage} crypto_cpu={} crypto_numa={:?} io_worker_cpu_numa={io:?}",
            crypto.cpu, crypto.numa_node
        );
    }
}

pub(super) fn size_workers(node: &Limits, plan: &mut AffinityPlan, rdma: bool) -> Result<Limits> {
    // Every I/O shard needs page progress reserves and, when enabled, a complete
    // native slot including aligned registered and staging buffers. Reduce shards
    // first, then rebalance shared crypto execution on the surviving NUMA nodes.
    let mut count = plan.pairs.len();
    let mut limiting_resource = None;
    loop {
        match partition_limits_with_cause(node, count, rdma) {
            Ok(limits) => {
                if count != plan.pairs.len() {
                    eprintln!(
                        "racer-dataplane: stage=worker-sizing planned_io={} final_io={count} limiting_resource={}",
                        plan.pairs.len(),
                        limiting_resource.unwrap_or("unknown")
                    );
                    plan.reduce_workers(count);
                }
                return Ok(limits);
            }
            Err((dimension, _)) if count > 1 => {
                // The last rejected count identifies a binding resource floor.
                limiting_resource = Some(dimension);
                count -= 1;
            }
            Err((dimension, error)) => {
                eprintln!(
                    "racer-dataplane: stage=worker-sizing io_workers={count} limiting_resource={dimension} error={error:?}"
                );
                return Err(error);
            }
        }
    }
}
