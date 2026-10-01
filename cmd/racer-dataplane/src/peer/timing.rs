//! Successful originating HTTP Page stage durations, not AEAD verification.
//! Failed, canceled, dropped, and non-Page outcomes publish only one censored count.
//! Eligibility begins at checkout, only for an HTTP plan (native plans, including
//! their fallback, are excluded). Auth includes admission and session reuse checks;
//! head spans send/receive, body spans staging admission and complete framed receipt.
//! Decode and forwarding verification gate all samples but are not timed stages.
use super::protocol::{Operation, PeerRequest, PeerResponse, VerifiedResponse};
use crate::{
    runtime::environment::now,
    telemetry::metrics::{Event, Metrics},
    topology::rails::TransportPlan,
};
use std::time::Instant;

pub(super) const STAGES: [(Event, Event); 4] = [
    (Event::PeerPageCheckoutCount, Event::PeerPageCheckoutNs),
    (Event::PeerPageAuthCount, Event::PeerPageAuthNs),
    (Event::PeerPageHeadCount, Event::PeerPageHeadNs),
    (Event::PeerPageBodyCount, Event::PeerPageBodyNs),
];

/// Stack-local state borrowed through transport awaits; never retains I/O resources.
pub(super) struct PageTiming<'a> {
    metrics: &'a Metrics,
    active: bool,
    start: Option<Instant>,
    durations: [u64; 4],
    completed: u8,
}
impl<'a> PageTiming<'a> {
    pub(super) fn new(metrics: &'a Metrics) -> Self {
        Self {
            metrics,
            active: false,
            start: None,
            durations: [0; 4],
            completed: 0,
        }
    }
    pub(super) fn enable(&mut self, request: &PeerRequest, plan: TransportPlan, opaque: bool) {
        self.active = !opaque
            && matches!(plan, TransportPlan::Http)
            && request.route.visited.len() == 1
            && matches!(request.operation, Operation::Page { .. });
        self.begin();
    }
    pub(super) fn begin(&mut self) {
        if self.active {
            self.start = Some(now());
        }
    }
    pub(super) fn end(&mut self, stage: usize) {
        if let Some(start) = self.start.take() {
            self.durations[stage] = now()
                .saturating_duration_since(start)
                .as_nanos()
                .min(u64::MAX as u128) as u64;
            self.completed |= 1 << stage;
        }
    }
    pub(super) fn success(&mut self, response: &VerifiedResponse) {
        if self.active
            && self.completed == 15
            && matches!(response.response(), PeerResponse::Page { .. })
        {
            // Publish together only after full framing, decode and forwarding verification.
            // Like all Metrics series, concurrent scrapes are not atomic snapshots.
            for ((count, sum), duration) in STAGES.into_iter().zip(self.durations) {
                let _ = self.metrics.record(sum, duration);
                let _ = self.metrics.record(count, 1);
            }
            self.active = false;
        }
    }
}
impl Drop for PageTiming<'_> {
    fn drop(&mut self) {
        if self.active {
            let _ = self.metrics.record(Event::PeerPageCensored, 1);
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn page_timing_duration_conversion_saturates_and_reversed_clock_is_zero() {
        use crate::runtime::environment::SimulationClock;
        use std::time::Duration;
        let metrics = Metrics::default();
        let clock = SimulationClock::new(9);
        let _environment = clock.environment(1).enter();
        let mut timing = PageTiming::new(&metrics);
        timing.active = true;
        timing.begin();
        clock.advance(Duration::from_secs(u64::MAX / 1_000_000_000 + 1));
        timing.end(0);
        assert_eq!(timing.durations[0], u64::MAX);
        timing.start = Some(now() + Duration::from_secs(1));
        timing.end(1);
        assert_eq!(timing.durations[1], 0);
        drop(timing);
        assert_eq!(metrics.count(Event::PeerPageCensored), 1);
        assert_eq!(metrics.count(Event::PeerPageCheckoutNs), 0);
    }
}
