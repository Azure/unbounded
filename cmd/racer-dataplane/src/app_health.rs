//! Readiness observations expire unless every required worker keeps progressing.
use super::*;
use crate::telemetry::health::{Health, Resources, State};
use std::{
    collections::HashMap,
    time::{SystemTime, UNIX_EPOCH},
};

#[derive(Default)]
pub(super) struct Observations {
    pub health: Health,
    workers: Mutex<HashMap<WorkerId, Resources>>,
}
impl Observations {
    fn record(&self, worker: WorkerId, resources: Resources, count: usize) -> Result<()> {
        let mut workers = self.workers.lock().map_err(|_| Error::Unavailable)?;
        workers.insert(worker, resources);
        let now = Instant::now();
        let complete = workers.len() == count;
        let all = |test: fn(&Resources) -> bool| {
            complete
                && workers
                    .values()
                    .all(|r| test(r) && r.observed_until.is_some_and(|until| now < until))
        };
        let resources = Resources {
            workers_usable: all(|r| r.workers_usable),
            storage_usable: all(|r| r.storage_usable),
            listeners_usable: all(|r| r.listeners_usable),
            membership_usable: all(|r| r.membership_usable),
            admission_usable: all(|r| r.admission_usable),
            credentials_valid_until: workers
                .values()
                .map(|r| r.credentials_valid_until)
                .min()
                .flatten(),
            observed_until: workers.values().map(|r| r.observed_until).min().flatten(),
        };
        self.health.observe(resources)?;
        if matches!(
            self.health.state()?,
            State::Starting | State::Ready | State::Degraded
        ) {
            self.health.transition(if resources.usable_at(now) {
                State::Ready
            } else {
                State::Degraded
            })?;
        }
        Ok(())
    }
}
impl WorkerApplication {
    pub(super) fn start_diagnostics(&mut self) -> Result<()> {
        if self.control.is_none() {
            return Ok(());
        }
        self.telemetry
            .attach_io(self.runtime.reactor.clone(), self.runtime.admission.clone())?;
        let diagnostic_scope = scope(Duration::from_secs(365 * 24 * 3600))?;
        let task_scope = diagnostic_scope.clone();
        let telemetry = self.telemetry.clone();
        let address = self.diagnostics_address;
        self.diagnostic_scope = Some(diagnostic_scope);
        self.diagnostic_task = Some(Box::pin(async move {
            telemetry.serve(address, &task_scope).await
        }));
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        if let Some(result) = poll_task(&mut self.diagnostic_task, &mut cx) {
            result?;
            return Err(Error::Unavailable);
        }
        Ok(())
    }
    pub(super) fn observe_health(&self) -> Result<()> {
        let Some(node) = &self.node else {
            return Ok(());
        };
        let now = Instant::now();
        let credentials = self.keys.signing_identity().ok().and_then(|identity| {
            let expiry = identity
                .certificate_chain()
                .iter()
                .filter_map(|der| {
                    let (_, cert) = x509_parser::parse_x509_certificate(der).ok()?;
                    u64::try_from(cert.validity().not_after.timestamp()).ok()
                })
                .min()?;
            let remaining = (UNIX_EPOCH + Duration::from_secs(expiry))
                .duration_since(SystemTime::now())
                .ok()?;
            now.checked_add(remaining)
        });
        let snapshot = self.snapshots.current().ok();
        node.observations.record(
            self.worker,
            Resources {
                workers_usable: self.started && !self.stopping,
                storage_usable: self.store.writer.slabs().alignment().is_ok(),
                listeners_usable: self.started
                    && (self.control.is_none()
                        || self.peer_task.is_some() && self.diagnostic_task.is_some()),
                membership_usable: snapshot
                    .as_ref()
                    .is_some_and(|s| self.snapshot_sequence == Some(s.sequence)),
                admission_usable: !self.runtime.admission.is_stopped() && !self.stopping,
                credentials_valid_until: credentials,
                observed_until: Some(now + Duration::from_secs(2)),
            },
            node.count,
        )
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn readiness_requires_every_worker_and_expires_without_progress() {
        let observations = Observations::default();
        let now = Instant::now();
        let resources = Resources {
            workers_usable: true,
            storage_usable: true,
            listeners_usable: true,
            membership_usable: true,
            admission_usable: true,
            credentials_valid_until: Some(now + Duration::from_secs(10)),
            observed_until: Some(now + Duration::from_secs(1)),
        };
        observations.record(WorkerId(0), resources, 2).unwrap();
        assert!(!observations.health.ready());
        observations.record(WorkerId(1), resources, 2).unwrap();
        assert!(observations.health.ready());
        assert_eq!(
            observations.health.state_at(now + Duration::from_secs(2)),
            Ok(State::Degraded)
        );
        observations
            .record(
                WorkerId(1),
                Resources {
                    storage_usable: false,
                    ..resources
                },
                2,
            )
            .unwrap();
        assert!(!observations.health.ready());
        observations.health.transition(State::Draining).unwrap();
        observations.record(WorkerId(1), resources, 2).unwrap();
        assert_eq!(observations.health.state(), Ok(State::Draining));
    }
}
