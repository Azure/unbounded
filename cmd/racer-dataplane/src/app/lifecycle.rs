//! Activate the assembled worker graph only after local resources and the first
//! complete publication are ready. Every wait keeps its original startup scope.
use super::*;
use crate::runtime::collections::HashMap;
use crate::telemetry::health::{Health, Resources, State};
use std::time::UNIX_EPOCH;

/// Readiness expires unless every required worker keeps progressing.
#[derive(Default)]
pub(super) struct Observations {
    pub health: Health,
    workers: Mutex<HashMap<WorkerId, Resources>>,
}
impl Observations {
    fn record(&self, worker: WorkerId, resources: Resources, count: usize) -> Result<()> {
        let mut workers = self.workers.lock().map_err(|_| Error::Unavailable)?;
        workers.insert(worker, resources);
        let now = crate::runtime::environment::now();
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
    pub(super) fn start_diagnostics(&mut self, cx: &mut Context<'_>) -> Result<()> {
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
        if let Some(result) = poll_task(&mut self.diagnostic_task, cx) {
            result?;
            return Err(Error::Unavailable);
        }
        Ok(())
    }
    pub(super) fn observe_health(&self) -> Result<()> {
        let node = &self.node;
        let now = crate::runtime::environment::now();
        if self.control.is_some() {
            self.telemetry.metrics.set_gauge(
                crate::telemetry::metrics::Gauge::KeyringGeneration,
                self.keys.generation()?.unwrap_or(0),
            );
        }
        let credentials = self.keys.signing_identity().ok().and_then(|identity| {
            let expiry = identity.expires_at_seconds();
            if self.control.is_some() {
                self.telemetry.metrics.set_gauge(
                    crate::telemetry::metrics::Gauge::IdentityExpiresAtSeconds,
                    expiry,
                );
            }
            let remaining = (UNIX_EPOCH + Duration::from_secs(expiry))
                .duration_since(crate::runtime::environment::wall_now())
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
    pub fn start<'a>(&'a mut self, startup: &'a RequestScope) -> Operation<'a, ()> {
        let environment = self.environment.clone();
        let drivers = self.drivers.clone();
        Box::pin(environment.scope(drivers.scope(async move {
            startup.check()?;
            if self.started || self.stopping {
                return Err(Error::InvalidConfiguration);
            }
            // The authenticated node identity was installed before this graph existed.
            self.keys.signing_identity()?;
            let alignment = self.store.writer.open().await?;
            let slabs = self.store.writer.slabs();
            let geometry = CheckpointGeometry::new(
                slabs.slab_bytes(),
                slabs.segment_bytes(),
                slabs.slab_bytes() / slabs.segment_bytes(),
                alignment,
            )?;
            self.store.recovery.configure_geometry(geometry)?;
            self.store.checkpoint.configure_geometry(geometry)?;
            let (payload, tail) = geometry.payload_capacity(
                self.store.eviction.reserve(),
                self.store.writer.index().page_capacity(),
            )?;
            self.telemetry
                .metrics
                .add_gauge(Gauge::EffectivePayloadBytes, payload);
            self.telemetry
                .metrics
                .add_gauge(Gauge::SegmentTailBytes, tail);
            self.telemetry.metrics.add_gauge(
                Gauge::DiskPageEntries,
                self.store.writer.index().page_capacity() as u64,
            );
            self.endpoint = Some(
                self.directory
                    .install(self.worker, self.coordinator.clone())?,
            );
            let node = self.node.clone();
            node.prepared.fetch_add(1, Ordering::Release);
            self.attach_cache_adapter();
            if let Some(control) = self.control.clone() {
                let identity = loop {
                    match control.start(startup).await {
                        Ok(identity) => break identity,
                        Err(
                            Error::Io
                            | Error::Unavailable
                            | Error::Overloaded
                            | Error::DeadlineExceeded,
                        ) => {
                            startup.check()?;
                            let io = ReactorControlIo::new(self.runtime.reactor.clone());
                            crate::control::transport::ControlIo::sleep(
                                &io,
                                control
                                    .next_attempt()
                                    .unwrap_or_else(crate::runtime::environment::now),
                                startup,
                            )
                            .await?;
                        }
                        Err(error) => return Err(error),
                    }
                };
                if identity.node() != self.keys.node() {
                    return Err(Error::NodeIdentityChanged);
                }
                control.activate_identity()?;
                // Accept a complete compatible first snapshot before any listener.
                while self.snapshots.current().is_err() {
                    startup.check()?;
                    let mut progress = control.progress(startup);
                    let result = std::future::poll_fn(|cx| {
                        self.poll_keyring(cx)?;
                        self.poll_cache_preparation(cx)?;
                        let result = progress.as_mut().poll(cx).map(|r| r.map(|_| ()));
                        if matches!(result, Poll::Ready(Err(_))) {
                            return result;
                        }
                        // Progress can install the prepared first snapshot while
                        // awaiting the next long poll. Startup depends on that
                        // local commit, not on another controller publication.
                        startup.check()?;
                        if self.snapshots.current().is_ok() {
                            Poll::Ready(Ok(()))
                        } else {
                            result
                        }
                    })
                    .await;
                    match result {
                        Ok(_) => (),
                        Err(
                            Error::Io
                            | Error::Unavailable
                            | Error::Overloaded
                            | Error::DeadlineExceeded,
                        ) => {
                            let io = ReactorControlIo::new(self.runtime.reactor.clone());
                            crate::control::transport::ControlIo::sleep(
                                &io,
                                crate::runtime::environment::now() + Duration::from_millis(100),
                                startup,
                            )
                            .await?;
                        }
                        Err(error) => return Err(error),
                    }
                }
            }
            std::future::poll_fn(|cx| {
                startup.check()?;
                self.poll_cache_preparation(cx)?;
                if STOP_REQUESTED.load(Ordering::Relaxed) {
                    return Poll::Ready(Err(Error::Cancelled));
                }
                if node.prepared.load(Ordering::Acquire) == node.count
                    && self.snapshots.current().is_ok()
                {
                    Poll::Ready(Ok(()))
                } else {
                    cx.waker().wake_by_ref();
                    Poll::Pending
                }
            })
            .await?;
            // Recovery must consult the accepted cache UID set, not only secrets.
            self.recover_node(geometry, startup).await?;
            self.refresh_snapshot(startup)?;
            self.activate_native(startup).await?;
            std::future::poll_fn(|cx| Poll::Ready(self.start_diagnostics(cx))).await?;
            self.task_scope = Some(scope(Duration::from_secs(365 * 24 * 3600))?);
            if self.control.is_some() {
                let listener_scope = scope(Duration::from_secs(365 * 24 * 3600))?;
                let peers = self.peers.clone();
                let peer_scope = listener_scope.clone();
                let address = self.peer_address;
                self.peer_task = Some(Box::pin(
                    async move { peers.listen(address, &peer_scope).await },
                ));
                self.listener_scope = Some(listener_scope);
                // The first poll actually binds the socket. A failed bind is a
                // startup error, never a successful readiness transition.
                if let Some(result) =
                    std::future::poll_fn(|cx| Poll::Ready(poll_task(&mut self.peer_task, cx))).await
                {
                    result?;
                    return Err(Error::Unavailable);
                }
            }
            self.started = true;
            self.observe_health()?;
            Ok(())
        })))
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn readiness_requires_every_worker_and_expires_without_progress() {
        let observations = Observations::default();
        let now = crate::runtime::environment::now();
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
