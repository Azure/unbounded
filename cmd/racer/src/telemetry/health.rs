use super::*;

#[derive(Clone, Default)]
pub struct Health(::telemetry::health::Health<Resources>);
pub use ::telemetry::health::State;
/// Complete worker observation; no identities or credentials enter health state.
#[derive(Clone, Copy, Debug, Default)]
pub struct Resources {
    pub workers_usable: bool,
    pub storage_usable: bool,
    pub listeners_usable: bool,
    pub membership_usable: bool,
    pub admission_usable: bool,
    pub credentials_valid_until: Option<Instant>,
    pub observed_until: Option<Instant>,
}
impl Resources {
    pub fn usable_at(&self, now: Instant) -> bool {
        self.workers_usable
            && self.storage_usable
            && self.listeners_usable
            && self.membership_usable
            && self.admission_usable
            && self
                .credentials_valid_until
                .is_some_and(|expiry| now < expiry)
            && self.observed_until.is_some_and(|expiry| now < expiry)
    }
}
impl Health {
    #[cfg(test)]
    pub fn state(&self) -> Result<State> {
        self.state_at(crate::test_support::clock().now())
    }
    pub fn state_at(&self, now: Instant) -> Result<State> {
        self.0
            .state_at(now, Resources::usable_at)
            .map_err(|_| Error::Unavailable)
    }
    pub fn observe(&self, resources: Resources) -> Result<()> {
        self.0.observe(resources).map_err(|_| Error::Unavailable)
    }
    #[cfg(test)]
    pub fn ready(&self) -> bool {
        self.ready_at(crate::test_support::clock().now())
    }
    pub fn ready_at(&self, now: Instant) -> bool {
        self.state_at(now).is_ok_and(|state| state == State::Ready)
    }
    #[cfg(test)]
    pub fn live(&self) -> bool {
        self.live_at(crate::test_support::clock().now())
    }
    /// Starting, degraded, and draining remain live; a response establishes progress.
    pub fn live_at(&self, now: Instant) -> bool {
        self.state_at(now)
            .is_ok_and(|state| state != State::Stopped)
    }
    #[cfg(test)]
    pub fn transition(&self, state: State) -> Result<()> {
        self.transition_at(state, crate::test_support::clock().now())
    }
    pub fn transition_at(&self, state: State, now: Instant) -> Result<()> {
        self.0
            .transition(state, |resources| resources.usable_at(now))
            .map_err(|_| Error::Unavailable)
    }
    /// Store an observation and settle Ready or Degraded atomically. Usability
    /// is judged at `now`, the instant the observation was aggregated.
    pub fn settle(&self, resources: Resources, now: Instant) -> Result<State> {
        self.0
            .settle(resources, |resources| resources.usable_at(now))
            .map_err(|_| Error::Unavailable)
    }
}

// Opt-in HTTP send tag checks, run on crypto, never payload scans on I/O.
// Shared limits: one active owner, two samples/second, 240 total, 120 seconds.
// Pending/unavailable samples do not prove agreement.

#[cfg(test)]
mod tests {
    use super::*;

    pub(crate) mod health_tests {
        use super::*;
        use std::time::Duration;
        #[test]
        fn lifecycle_probes_keep_resource_policy_bodies_and_irreversible_drain() {
            let telemetry = Telemetry::default();
            let now = racer_io::Clock::host().now();
            let usable = Resources {
                workers_usable: true,
                storage_usable: true,
                listeners_usable: true,
                membership_usable: true,
                admission_usable: true,
                credentials_valid_until: Some(now + Duration::from_secs(60)),
                observed_until: Some(now + Duration::from_secs(60)),
            };
            let probe = |path, admission, expected, available: bool| {
                let mut body = String::new();
                let response = get(&telemetry, path, admission, &mut body).unwrap();
                assert_eq!(body, expected);
                assert_eq!(matches!(response, Response::Text), available);
                assert_eq!(matches!(response, Response::Unavailable), !available);
            };
            probe("/healthz", true, "ok\n", true);
            probe("/readyz", true, "not ready\n", false);
            assert_eq!(
                telemetry.health.transition(State::Ready),
                Err(Error::Unavailable)
            );
            telemetry.health.observe(usable).unwrap();
            telemetry.health.transition(State::Ready).unwrap();
            probe("/readyz", true, "ready\n", true);
            probe("/readyz", false, "not ready\n", false);
            telemetry
                .health
                .observe(Resources {
                    storage_usable: false,
                    ..usable
                })
                .unwrap();
            assert_eq!(telemetry.health.state(), Ok(State::Degraded));
            probe("/healthz", true, "ok\n", true);
            probe("/readyz", true, "not ready\n", false);
            telemetry.health.observe(usable).unwrap();
            assert_eq!(telemetry.health.state(), Ok(State::Ready));
            telemetry.health.transition(State::Draining).unwrap();
            assert_eq!(
                telemetry.health.transition(State::Ready),
                Err(Error::Unavailable)
            );
            probe("/healthz", true, "ok\n", true);
            probe("/readyz", true, "not ready\n", false);
            telemetry.health.transition(State::Stopped).unwrap();
            assert_eq!(
                telemetry.health.transition(State::Starting),
                Err(Error::Unavailable)
            );
            probe("/healthz", true, "not live\n", false);
            assert_eq!(telemetry.metrics.count(Event::DiagnosticHealth), 4);
            assert_eq!(telemetry.metrics.count(Event::DiagnosticReady), 5);
        }
        #[test]
        fn observation_and_credential_expiry_are_inclusive() {
            let now = racer_io::Clock::host().now();
            for (observed, boundary) in [(5, 5), (20, 10)] {
                let health = Health::default();
                health
                    .observe(Resources {
                        workers_usable: true,
                        storage_usable: true,
                        listeners_usable: true,
                        membership_usable: true,
                        admission_usable: true,
                        credentials_valid_until: Some(now + Duration::from_secs(10)),
                        observed_until: Some(now + Duration::from_secs(observed)),
                    })
                    .unwrap();
                health.transition(State::Ready).unwrap();
                assert_eq!(
                    health
                        .state_at(now + Duration::from_secs(boundary) - Duration::from_nanos(1))
                        .unwrap(),
                    State::Ready
                );
                assert_eq!(
                    health
                        .state_at(now + Duration::from_secs(boundary))
                        .unwrap(),
                    State::Degraded
                );
            }
        }
    }
}
