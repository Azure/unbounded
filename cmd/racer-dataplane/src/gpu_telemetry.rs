//! Prometheus metrics for `gpu-read`, cuFile, and GPU RDMA landing.
//!
//! The GPU helper and the RDMA link node keep their own counters and state.
//! [`GpuTelemetry`] holds handles to both, attached once at startup, and reads
//! them when `/metrics` renders. Request outcomes are counted here because the
//! client listener sees them, including on nodes with no GPU helper.
//!
//! Labels are bounded: request outcomes and cuFile reasons are fixed sets, GPU
//! labels come from the GPUs found at startup, and rail labels from the active
//! RDMA rails.

use crate::error::Error;
use crate::gpu::CufileDisable;
use crate::gpu::GpuHelper;
use crate::rdma::LinkNode;
use std::fmt::Write;
use std::sync::Arc;
use std::sync::OnceLock;
use std::sync::atomic::AtomicU64;
use std::sync::atomic::Ordering;

/// How the dataplane answered a `gpu-read` request head.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum GpuReadOutcome {
    /// The allocation was imported on a GPU and the read proceeds.
    Gpu,
    /// `gpu-read` is off on this node (501).
    NotEnabled,
    /// The allocation is on a GPU the dataplane cannot use, or the
    /// `Racer-Gpu-Uuid` header names a GPU it did not find (501).
    WrongDevice,
    /// Too many open requests or no GPU memory (503).
    Busy,
    /// The request or its handle was invalid (400).
    Invalid,
    /// Any other failure.
    Failed,
}

impl GpuReadOutcome {
    pub const ALL: [Self; 6] = [
        Self::Gpu,
        Self::NotEnabled,
        Self::WrongDevice,
        Self::Busy,
        Self::Invalid,
        Self::Failed,
    ];

    /// Metric label value.
    pub fn label(self) -> &'static str {
        match self {
            Self::Gpu => "gpu",
            Self::NotEnabled => "not_enabled",
            Self::WrongDevice => "wrong_device",
            Self::Busy => "busy",
            Self::Invalid => "invalid",
            Self::Failed => "failed",
        }
    }

    /// Classify the result of opening a `gpu-read` sink. `gpu` says whether
    /// this node serves `gpu-read` at all.
    pub fn classify(result: Result<(), Error>, gpu: bool) -> Self {
        match result {
            Ok(()) => Self::Gpu,
            Err(Error::NotImplemented) if !gpu => Self::NotEnabled,
            Err(Error::NotImplemented) => Self::WrongDevice,
            Err(Error::Overloaded) => Self::Busy,
            Err(Error::InvalidRequest | Error::InvalidRange) => Self::Invalid,
            Err(_) => Self::Failed,
        }
    }
}

/// Samples written without a GPU helper: one per request outcome, plus
/// `racer_gpu_enabled`.
pub const BASE_SERIES: usize = GpuReadOutcome::ALL.len() + 1;

/// GPU metric sources shared by every worker's metrics handle.
#[derive(Default)]
pub struct GpuTelemetry {
    helper: OnceLock<Arc<GpuHelper>>,
    links: OnceLock<Arc<LinkNode>>,
    requests: [AtomicU64; GpuReadOutcome::ALL.len()],
}

impl GpuTelemetry {
    /// Report this GPU helper. Later calls are ignored.
    pub fn attach_helper(&self, helper: Arc<GpuHelper>) {
        let _ = self.helper.set(helper);
    }

    /// Report GPU landing state from this link node. Later calls are ignored.
    pub fn attach_links(&self, links: Arc<LinkNode>) {
        let _ = self.links.set(links);
    }

    /// Count one `gpu-read` request head.
    pub fn record_request(&self, outcome: GpuReadOutcome) {
        let counter = &self.requests[outcome as usize];
        let _ = counter.fetch_update(Ordering::Relaxed, Ordering::Relaxed, |v| {
            Some(v.saturating_add(1))
        });
    }

    /// Requests counted with `outcome`.
    pub fn requests(&self, outcome: GpuReadOutcome) -> u64 {
        self.requests[outcome as usize].load(Ordering::Relaxed)
    }

    /// Write every GPU series in Prometheus text format.
    pub fn write(&self, out: &mut impl Write) -> std::fmt::Result {
        writeln!(out, "# TYPE racer_gpu_read_requests_total counter")?;
        for outcome in GpuReadOutcome::ALL {
            writeln!(
                out,
                "racer_gpu_read_requests_total{{outcome=\"{}\"}} {}",
                outcome.label(),
                self.requests(outcome)
            )?;
        }
        let helper = self.helper.get();
        writeln!(
            out,
            "# TYPE racer_gpu_enabled gauge\nracer_gpu_enabled {}",
            u8::from(helper.is_some())
        )?;
        let Some(helper) = helper else {
            return Ok(());
        };
        writeln!(out, "# TYPE racer_gpu_decrypt_available gauge")?;
        for device in helper.devices() {
            writeln!(
                out,
                "racer_gpu_decrypt_available{{ordinal=\"{}\",uuid=\"{}\"}} {}",
                device.ordinal,
                device.uuid_string(),
                u8::from(helper.decrypts(device.ordinal))
            )?;
        }
        writeln!(out, "# TYPE racer_gpu_staging_slots_free gauge")?;
        for device in helper.devices() {
            writeln!(
                out,
                "racer_gpu_staging_slots_free{{ordinal=\"{}\"}} {}",
                device.ordinal,
                helper.free_slots(device.ordinal)
            )?;
        }
        let counters = helper.counters();
        let load = |counter: &AtomicU64| counter.load(Ordering::Relaxed);
        writeln!(
            out,
            "# TYPE racer_gpu_cufile_enabled gauge\nracer_gpu_cufile_enabled {}",
            u8::from(helper.cufile())
        )?;
        writeln!(out, "# TYPE racer_gpu_cufile_disabled_total counter")?;
        for reason in CufileDisable::ALL {
            writeln!(
                out,
                "racer_gpu_cufile_disabled_total{{reason=\"{}\"}} {}",
                reason.label(),
                load(&counters.cufile_disabled[reason as usize])
            )?;
        }
        writeln!(out, "# TYPE racer_gpu_read_pages_total counter")?;
        for (decrypt, value) in [
            ("gpu", load(&counters.decrypted)),
            ("cpu", load(&counters.plaintext)),
        ] {
            writeln!(
                out,
                "racer_gpu_read_pages_total{{decrypt=\"{decrypt}\"}} {value}"
            )?;
        }
        for (name, kind, value) in [
            (
                "racer_gpu_pages_rejected_total",
                "counter",
                load(&counters.rejected),
            ),
            (
                "racer_gpu_cufile_staged_pages_total",
                "counter",
                load(&counters.staged),
            ),
            (
                "racer_gpu_cufile_failures_total",
                "counter",
                load(&counters.file_failures),
            ),
            (
                "racer_gpu_staging_exhausted_total",
                "counter",
                load(&counters.exhausted),
            ),
            (
                "racer_gpu_landed_pages_decrypted_total",
                "counter",
                load(&counters.landed),
            ),
            (
                "racer_gpu_cufile_unsupported_total",
                "counter",
                load(&counters.cufile_unsupported),
            ),
            (
                "racer_gpu_open_requests",
                "gauge",
                helper.open_requests() as u64,
            ),
            ("racer_gpu_leaked_resources", "gauge", helper.leaked()),
        ] {
            writeln!(out, "# TYPE {name} {kind}\n{name} {value}")?;
        }
        if let Some(links) = self.links.get()
            && links.gpu_landing_configured()
        {
            let rails = links.landing_state();
            writeln!(out, "# TYPE racer_gpu_landing_enabled gauge")?;
            for (rail, state) in &rails {
                writeln!(
                    out,
                    "racer_gpu_landing_enabled{{rail=\"{rail}\"}} {}",
                    u8::from(!state.is_empty())
                )?;
            }
            writeln!(out, "# TYPE racer_gpu_landing_slots gauge")?;
            for (rail, state) in &rails {
                for (ordinal, slots) in state {
                    writeln!(
                        out,
                        "racer_gpu_landing_slots{{rail=\"{rail}\",ordinal=\"{ordinal}\"}} {slots}"
                    )?;
                }
            }
            writeln!(out, "# TYPE racer_gpu_landing_exhausted_total counter")?;
            for ((rail, ordinal), count) in links.landing_exhausted() {
                writeln!(
                    out,
                    "racer_gpu_landing_exhausted_total{{rail=\"{rail}\",ordinal=\"{ordinal}\"}} {count}"
                )?;
            }
        }
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn render(telemetry: &GpuTelemetry) -> String {
        let mut out = String::new();
        telemetry.write(&mut out).unwrap();
        out
    }

    #[test]
    fn classify_maps_errors_to_bounded_outcomes() {
        use GpuReadOutcome as O;
        assert_eq!(O::classify(Ok(()), true), O::Gpu);
        assert_eq!(
            O::classify(Err(Error::NotImplemented), false),
            O::NotEnabled
        );
        assert_eq!(
            O::classify(Err(Error::NotImplemented), true),
            O::WrongDevice
        );
        assert_eq!(O::classify(Err(Error::Overloaded), true), O::Busy);
        assert_eq!(O::classify(Err(Error::InvalidRequest), true), O::Invalid);
        assert_eq!(O::classify(Err(Error::InvalidRange), true), O::Invalid);
        assert_eq!(O::classify(Err(Error::Unavailable), true), O::Failed);
    }

    #[test]
    fn without_a_helper_only_requests_and_enabled_render() {
        let telemetry = GpuTelemetry::default();
        telemetry.record_request(GpuReadOutcome::NotEnabled);
        telemetry.record_request(GpuReadOutcome::NotEnabled);
        let text = render(&telemetry);
        assert!(text.contains("racer_gpu_read_requests_total{outcome=\"not_enabled\"} 2\n"));
        assert!(text.contains("racer_gpu_read_requests_total{outcome=\"gpu\"} 0\n"));
        assert!(text.contains("racer_gpu_enabled 0\n"));
        assert!(!text.contains("racer_gpu_cufile_enabled"));
        assert!(!text.contains("racer_gpu_landing"));
        assert_eq!(text.matches("# TYPE").count(), 2);
        assert_eq!(
            text.lines().filter(|l| !l.starts_with('#')).count(),
            BASE_SERIES
        );
    }

    #[test]
    fn request_counter_saturates() {
        let telemetry = GpuTelemetry::default();
        telemetry.requests[0].store(u64::MAX, Ordering::Relaxed);
        telemetry.record_request(GpuReadOutcome::Gpu);
        assert_eq!(telemetry.requests(GpuReadOutcome::Gpu), u64::MAX);
    }

    #[test]
    fn with_a_helper_every_gpu_series_renders() {
        let helper = crate::gpu::testing::helper(&[None]);
        helper.counters().decrypted.fetch_add(3, Ordering::Relaxed);
        helper.counters().plaintext.fetch_add(2, Ordering::Relaxed);
        helper.counters().cufile_disabled(CufileDisable::FileOpen);
        let telemetry = GpuTelemetry::default();
        telemetry.attach_helper(helper.clone());
        let links = Arc::new(LinkNode::default());
        telemetry.attach_links(links.clone());
        let text = render(&telemetry);
        for name in [
            "racer_gpu_read_requests_total",
            "racer_gpu_enabled",
            "racer_gpu_decrypt_available",
            "racer_gpu_staging_slots_free",
            "racer_gpu_cufile_enabled",
            "racer_gpu_cufile_disabled_total",
            "racer_gpu_read_pages_total",
            "racer_gpu_pages_rejected_total",
            "racer_gpu_cufile_staged_pages_total",
            "racer_gpu_cufile_failures_total",
            "racer_gpu_staging_exhausted_total",
            "racer_gpu_landed_pages_decrypted_total",
            "racer_gpu_cufile_unsupported_total",
            "racer_gpu_open_requests",
            "racer_gpu_leaked_resources",
        ] {
            assert!(text.contains(&format!("# TYPE {name} ")), "{name}\n{text}");
        }
        assert!(text.contains("racer_gpu_enabled 1\n"));
        assert!(text.contains("racer_gpu_read_pages_total{decrypt=\"gpu\"} 3\n"));
        assert!(text.contains("racer_gpu_read_pages_total{decrypt=\"cpu\"} 2\n"));
        assert!(text.contains("racer_gpu_cufile_disabled_total{reason=\"file_open\"} 1\n"));
        assert!(text.contains("racer_gpu_cufile_disabled_total{reason=\"driver_open\"} 0\n"));
        let uuid = helper.devices()[0].uuid_string();
        assert!(text.contains(&format!(
            "racer_gpu_decrypt_available{{ordinal=\"0\",uuid=\"{uuid}\"}} 1\n"
        )));
        // Landing is not configured on this link node.
        assert!(!text.contains("racer_gpu_landing_enabled"));
        links.set_gpu_landing(helper, 1 << 30);
        let text = render(&telemetry);
        assert!(text.contains("# TYPE racer_gpu_landing_enabled gauge\n"));
        assert!(text.contains("# TYPE racer_gpu_landing_slots gauge\n"));
        assert!(text.contains("# TYPE racer_gpu_landing_exhausted_total counter\n"));
        links.count_landing_exhausted(racer_control_wire::RailId(2), 0);
        links.count_landing_exhausted(racer_control_wire::RailId(2), 0);
        links.count_landing_exhausted(racer_control_wire::RailId(1), 3);
        let text = render(&telemetry);
        assert!(text.contains(
            "racer_gpu_landing_exhausted_total{rail=\"1\",ordinal=\"3\"} 1\n\
             racer_gpu_landing_exhausted_total{rail=\"2\",ordinal=\"0\"} 2\n"
        ));
    }
}
