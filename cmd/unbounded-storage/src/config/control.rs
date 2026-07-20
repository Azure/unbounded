// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! The single funnel through which every configuration change is
//! applied to the running process.
//!
//! [`ConfigController`] turns a freshly loaded [`Config`] into a
//! *blocking* apply: the call returns only once the new configuration
//! has been fully realized across every shard. This is the "close the
//! loop" property. The config file watcher drives it: each successful
//! reparse is pushed through [`ConfigController::apply`], which blocks
//! until the process has converged onto it.
//!
//! Every applicable change is realized in place: peers, disks, routing,
//! backends, and frontends are all reconciled on the live shard layer
//! without ever tearing it down or restarting the process. Routing is
//! republished through the shared [`RouteTableHandle`], and each shard
//! reconciles its own backend and frontend registries from the
//! broadcast config on its own thread. Startup-fixed knobs (the
//! `[startup]` section: memory, fabric thread/endpoint/in-flight, and
//! CPU topology settings) are not part of the dynamic apply path and
//! only take effect on process start; the version whose startup knobs
//! are realized is tracked separately as the startup config version.
//!
//! The controller itself is deliberately decoupled from the shard
//! machinery: it owns only the current config, the latest-known,
//! latest-applied, and startup config versions
//! ([`ConfigVersionStatus`]), and a
//! [`ConfigApplyTarget`] that the binary implements. The reusable
//! blocking fan-out/fan-in primitive used by that implementation lives
//! here as [`ShardControlGroup`].

use std::collections::HashSet;
use std::fmt;
use std::sync::Arc;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::mpsc::{self, RecvTimeoutError, Sender};
use std::time::{Duration, Instant};

use crate::config::{ConfigDiff, LoadedConfig};
use crate::runtime::WorkerIdx;

/// A command delivered to a single shard's control channel. The shard
/// drains its receiver from a tick hook on its own thread, applies the
/// command there (so all `!Send` per-shard state stays thread-local),
/// then acknowledges.
pub enum ShardCommand {
    /// Apply a new configuration in place: republish routing and
    /// reconcile this shard's backend registry, frontend registry, and
    /// any disk-policy side effects.
    ApplyConfig(ShardApply),
    /// Drop all retained in-memory page-cache entries after process-level
    /// disk policy publication has landed.
    DrainPageCache(ShardDrainPageCache),
}

/// Payload of [`ShardCommand::DrainPageCache`].
pub struct ShardDrainPageCache {
    /// Channel the shard sends its [`ShardAck`] on once the drain has
    /// completed on the shard thread.
    pub ack: Sender<ShardAck>,
}

/// Payload of [`ShardCommand::ApplyConfig`].
pub struct ShardApply {
    /// The new finalized config, runtime graph, and route table.
    pub loaded: Arc<LoadedConfig>,
    /// Section-level diff that selected this apply path.
    pub diff: ConfigDiff,
    /// Channel the shard sends its [`ShardAck`] on once the apply has
    /// completed on the shard thread.
    pub ack: Sender<ShardAck>,
}

/// A shard's acknowledgement that it finished (or failed) applying a
/// [`ShardCommand`].
pub struct ShardAck {
    pub worker: WorkerIdx,
    pub result: Result<(), String>,
}

/// Errors that abort a blocking apply.
#[derive(Debug)]
pub enum ApplyError {
    /// A shard control channel was already closed when we tried to send
    /// it a command (the shard thread is gone).
    ShardSend(WorkerIdx),
    /// The ack channel disconnected before every shard reported, so we
    /// cannot prove the apply completed everywhere.
    AckDisconnected {
        expected: usize,
        received: usize,
        outstanding: Vec<WorkerIdx>,
    },
    /// Not every shard acknowledged before the control deadline. Commands
    /// already delivered may still complete, so the live process can no
    /// longer safely retry the apply.
    AckTimeout {
        expected: usize,
        received: usize,
        outstanding: Vec<WorkerIdx>,
    },
    /// One or more shards reported a failure while applying.
    ShardApply(Vec<(WorkerIdx, String)>),
    /// The process-level apply target rejected the config.
    Target(String),
}

impl fmt::Display for ApplyError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            ApplyError::ShardSend(w) => {
                write!(f, "shard {} control channel closed before apply", w.0)
            }
            ApplyError::AckDisconnected {
                expected,
                received,
                outstanding,
            } => write!(
                f,
                "ack channel disconnected after {received}/{expected} shards reported; outstanding workers: {}",
                format_workers(outstanding),
            ),
            ApplyError::AckTimeout {
                expected,
                received,
                outstanding,
            } => write!(
                f,
                "ack deadline expired after {received}/{expected} shards reported; outstanding workers: {}",
                format_workers(outstanding),
            ),
            ApplyError::ShardApply(failures) => {
                write!(f, "{} shard(s) failed to apply config:", failures.len())?;
                for (w, e) in failures {
                    write!(f, " [shard {}: {e}]", w.0)?;
                }
                Ok(())
            }
            ApplyError::Target(e) => write!(f, "apply target rejected config: {e}"),
        }
    }
}

impl std::error::Error for ApplyError {}

impl ApplyError {
    /// Whether some shards may still apply a command after this error is
    /// returned. The process must stop rather than retry against its old
    /// controller snapshot.
    pub fn apply_state_is_indeterminate(&self) -> bool {
        matches!(
            self,
            Self::ShardSend(_) | Self::AckDisconnected { .. } | Self::AckTimeout { .. }
        )
    }
}

/// Which path an apply took. Returned so callers (and tests) can assert
/// on the work that actually happened.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ApplyTier {
    /// The new config was byte-identical to the running one; nothing was
    /// applied.
    NoChange,
    /// In-place peer/disk/routing/backend/frontend reconcile.
    InPlace,
}

/// Result of a completed [`ConfigController::apply`].
#[derive(Debug, Clone, Copy)]
pub struct ApplyOutcome {
    pub tier: ApplyTier,
    pub diff: ConfigDiff,
}

/// A point-in-time read of the daemon's top-level config versions.
///
/// `known` is the [`Config::version`] of the most recent configuration
/// the process has successfully loaded; `applied` is the version of the
/// most recent configuration it has *fully* realized across every shard.
/// They are equal once the process has converged onto the latest config
/// the operator pushed, and `applied` lags `known` while an apply is in
/// flight or after one has failed.
///
/// `startup` is the [`Config::version`] of the configuration whose
/// startup-fixed knobs (the `[startup]` section: memory, fabric, and
/// CPU topology settings) are currently realized by the process. Those
/// knobs only take effect on process start and are deliberately
/// excluded from the dynamic apply path, so `startup` is seeded once at
/// construction and never advances: it lags `known`/`applied` whenever a
/// later config changed a startup-only knob that a restart has not yet
/// picked up. All three are plumbed through ready to be published as
/// gauge metrics; nothing exposes them yet.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct ConfigVersionSnapshot {
    pub known: u64,
    pub applied: u64,
    pub startup: u64,
}

/// A cloneable, shareable handle to the daemon's top-level config
/// versions.
///
/// The [`ConfigController`] advances `known` as soon as it receives a
/// loaded config (before the apply runs) and `applied` only once that
/// config has fully converged (or was a no-op against the already-running
/// config). `startup` is seeded from the config realized at process
/// start and never advances, since the startup-fixed knobs it tracks
/// only take effect on restart. Backed by atomics so reads never block
/// an in-flight apply. This is the surface a future metrics exporter
/// will read to publish the latest-known, latest-applied, and startup
/// config versions as gauges; nothing reads it yet.
#[derive(Clone)]
pub struct ConfigVersionStatus {
    known: Arc<AtomicU64>,
    applied: Arc<AtomicU64>,
    startup: Arc<AtomicU64>,
}

impl ConfigVersionStatus {
    /// Seed all three versions with the config version realized at
    /// startup. `known` and `applied` advance as later configs are
    /// loaded and applied; `startup` stays fixed at this value for the
    /// life of the process.
    pub fn new(initial: u64) -> Self {
        Self {
            known: Arc::new(AtomicU64::new(initial)),
            applied: Arc::new(AtomicU64::new(initial)),
            startup: Arc::new(AtomicU64::new(initial)),
        }
    }

    /// Record that `version` is the latest configuration the process has
    /// loaded. Called by the controller as soon as a config is received,
    /// before the apply runs, so a failed apply still advances the
    /// latest-known version.
    fn record_known(&self, version: u64) {
        self.known.store(version, Ordering::Release);
    }

    /// Record that `version` has been fully realized. Called by the
    /// controller only after a successful (or no-op) apply.
    fn record_applied(&self, version: u64) {
        self.applied.store(version, Ordering::Release);
    }

    /// The version of the most recently loaded configuration.
    pub fn known(&self) -> u64 {
        self.known.load(Ordering::Acquire)
    }

    /// The version of the most recently fully-applied configuration.
    pub fn applied(&self) -> u64 {
        self.applied.load(Ordering::Acquire)
    }

    /// The version of the configuration whose startup-fixed knobs are
    /// currently realized. Seeded at process start and never advanced.
    pub fn startup(&self) -> u64 {
        self.startup.load(Ordering::Acquire)
    }

    /// A point-in-time snapshot of all three versions.
    pub fn snapshot(&self) -> ConfigVersionSnapshot {
        ConfigVersionSnapshot {
            known: self.known(),
            applied: self.applied(),
            startup: self.startup(),
        }
    }
}

/// The process-specific side of an apply. The binary implements this
/// over its shard fabrics, disk registry, and shard control group; the
/// controller stays free of all of it so it can be unit tested against a
/// mock.
///
/// The method is blocking: it must not return until the new
/// configuration is fully realized (or an error is produced).
pub trait ConfigApplyTarget {
    /// Apply a change in place. `diff` reports exactly which sections
    /// changed so the implementation can skip untouched work (e.g. only
    /// reconcile fabric peers when `requires_routing_reload`).
    fn apply_in_place(
        &mut self,
        new: &Arc<LoadedConfig>,
        diff: &ConfigDiff,
    ) -> Result<(), ApplyError>;
}

/// The single funnel for configuration changes. Holds the currently
/// applied config and delegates the process-specific work to a
/// [`ConfigApplyTarget`].
pub struct ConfigController<T: ConfigApplyTarget> {
    target: T,
    current: Arc<LoadedConfig>,
    versions: ConfigVersionStatus,
}

impl<T: ConfigApplyTarget> ConfigController<T> {
    /// Create a controller seeded with the configuration already applied
    /// to the process at startup. The latest-known, latest-applied, and
    /// startup config versions are all seeded from `initial`'s
    /// [`Config::version`].
    pub fn new(target: T, initial: Arc<LoadedConfig>) -> Self {
        let versions = ConfigVersionStatus::new(initial.config().version);
        Self {
            target,
            current: initial,
            versions,
        }
    }

    /// The configuration currently realized by the process.
    pub fn current(&self) -> &Arc<LoadedConfig> {
        &self.current
    }

    /// A cloneable handle to the daemon's latest-known, latest-applied,
    /// and startup config versions. Hand this to a metrics exporter (none
    /// exists yet) to publish them as gauges; the controller keeps
    /// known/applied advanced as configs are loaded and applied, while
    /// startup stays pinned to the version realized at process start.
    pub fn config_versions(&self) -> ConfigVersionStatus {
        self.versions.clone()
    }

    /// Borrow the underlying target (used by the binary to keep driving
    /// the shard layer between applies).
    pub fn target_mut(&mut self) -> &mut T {
        &mut self.target
    }

    /// Consume the controller and return the underlying target. Used at
    /// shutdown to reclaim the shard layer (and disk registry) the
    /// target owns so teardown can run in the right order.
    pub fn into_target(self) -> T {
        self.target
    }

    /// Apply `new`, blocking until the process has fully converged.
    ///
    /// Classifies the change via [`ConfigDiff`], runs the matching tier,
    /// and only advances the recorded current config once the tier has
    /// completed successfully. On error the current config is left
    /// unchanged so a subsequent apply re-derives the same diff and can
    /// retry.
    ///
    /// The latest-known config version is advanced to `new`'s
    /// [`Config::version`] up front, before any work, so it reflects every
    /// config the process has loaded even if the apply below fails. The
    /// latest-applied config version is advanced to the same value only on
    /// full success - including a no-op apply against the already-running
    /// config - so observers can tell the process has converged onto it.
    pub fn apply(&mut self, new: Arc<LoadedConfig>) -> Result<ApplyOutcome, ApplyError> {
        // Record the loaded version before doing any work so the
        // latest-known version advances even if the apply fails below.
        self.versions.record_known(new.config().version);

        let diff = ConfigDiff::between_loaded(&self.current, &new);

        if !diff.any() {
            self.versions.record_applied(new.config().version);
            self.current = new;
            return Ok(ApplyOutcome {
                tier: ApplyTier::NoChange,
                diff,
            });
        }

        self.target.apply_in_place(&new, &diff)?;

        self.versions.record_applied(new.config().version);
        self.current = new;
        Ok(ApplyOutcome {
            tier: ApplyTier::InPlace,
            diff,
        })
    }
}

/// A reusable blocking fan-out/fan-in primitive over a set of shard
/// control channels.
///
/// [`broadcast_apply`](Self::broadcast_apply) sends one
/// [`ShardCommand::ApplyConfig`] to every shard and blocks until each
/// has acknowledged or the group's absolute deadline expires, so the caller
/// can guarantee the change has landed on every shard thread before returning
/// success. This is the concrete
/// "close the loop" mechanism a [`ConfigApplyTarget`] builds its Tier 1
/// path on.
pub struct ShardControlGroup {
    senders: Vec<(WorkerIdx, Sender<ShardCommand>)>,
    ack_timeout: Duration,
}

impl ShardControlGroup {
    pub fn new(senders: Vec<(WorkerIdx, Sender<ShardCommand>)>, ack_timeout: Duration) -> Self {
        Self {
            senders,
            ack_timeout,
        }
    }

    pub fn len(&self) -> usize {
        self.senders.len()
    }

    pub fn is_empty(&self) -> bool {
        self.senders.is_empty()
    }

    /// Send one loaded snapshot to every shard and block until all of
    /// them acknowledge.
    ///
    /// Returns `Ok(())` only when every shard reports success. If a
    /// channel is closed at send time, the ack deadline expires, the ack
    /// channel disconnects before all shards report, or any shard reports a
    /// failure, the corresponding [`ApplyError`] is returned. Until a terminal
    /// channel/deadline error, fan-in keeps collecting reports so one failing
    /// shard does not mask failures from the others.
    pub fn broadcast_apply(
        &self,
        loaded: Arc<LoadedConfig>,
        diff: ConfigDiff,
    ) -> Result<(), ApplyError> {
        self.broadcast(|ack| {
            ShardCommand::ApplyConfig(ShardApply {
                loaded: loaded.clone(),
                diff,
                ack,
            })
        })
    }

    /// Ask every shard to drain retained RAM page-cache entries and block
    /// until all have acknowledged.
    pub fn broadcast_drain_page_cache(&self) -> Result<(), ApplyError> {
        self.broadcast(|ack| ShardCommand::DrainPageCache(ShardDrainPageCache { ack }))
    }

    fn broadcast<F>(&self, make_cmd: F) -> Result<(), ApplyError>
    where
        F: Fn(Sender<ShardAck>) -> ShardCommand,
    {
        let mut outstanding: HashSet<WorkerIdx> =
            self.senders.iter().map(|(worker, _)| *worker).collect();
        let expected = outstanding.len();
        if expected == 0 {
            return Ok(());
        }

        let (ack_tx, ack_rx) = mpsc::channel::<ShardAck>();
        for (worker, sender) in &self.senders {
            sender
                .send(make_cmd(ack_tx.clone()))
                .map_err(|_| ApplyError::ShardSend(*worker))?;
        }
        drop(ack_tx);

        let deadline = Instant::now() + self.ack_timeout;
        let mut received = 0usize;
        let mut failures = Vec::new();
        while !outstanding.is_empty() {
            let Some(remaining) = deadline.checked_duration_since(Instant::now()) else {
                return Err(ApplyError::AckTimeout {
                    expected,
                    received,
                    outstanding: sorted_workers(&outstanding),
                });
            };
            match ack_rx.recv_timeout(remaining) {
                Ok(ack) => {
                    if outstanding.remove(&ack.worker) {
                        received += 1;
                        if let Err(e) = ack.result {
                            failures.push((ack.worker, e));
                        }
                    }
                }
                Err(RecvTimeoutError::Timeout) => {
                    return Err(ApplyError::AckTimeout {
                        expected,
                        received,
                        outstanding: sorted_workers(&outstanding),
                    });
                }
                Err(RecvTimeoutError::Disconnected) => {
                    return Err(ApplyError::AckDisconnected {
                        expected,
                        received,
                        outstanding: sorted_workers(&outstanding),
                    });
                }
            }
        }

        if failures.is_empty() {
            Ok(())
        } else {
            Err(ApplyError::ShardApply(failures))
        }
    }
}

fn sorted_workers(workers: &HashSet<WorkerIdx>) -> Vec<WorkerIdx> {
    let mut workers: Vec<_> = workers.iter().copied().collect();
    workers.sort_by_key(|worker| worker.0);
    workers
}

fn format_workers(workers: &[WorkerIdx]) -> String {
    workers
        .iter()
        .map(|worker| worker.0.to_string())
        .collect::<Vec<_>>()
        .join(",")
}

#[cfg(test)]
mod tests {
    use std::sync::Arc;
    use std::sync::mpsc;
    use std::thread;
    use std::time::Duration;

    use super::*;
    use crate::config::Config;
    fn loaded(config: Config) -> Arc<LoadedConfig> {
        Arc::new(LoadedConfig::from_config(config).unwrap())
    }

    fn control_group(senders: Vec<(WorkerIdx, Sender<ShardCommand>)>) -> ShardControlGroup {
        ShardControlGroup::new(senders, Duration::from_secs(1))
    }

    fn ack_sender(cmd: ShardCommand) -> Sender<ShardAck> {
        match cmd {
            ShardCommand::ApplyConfig(apply) => apply.ack,
            ShardCommand::DrainPageCache(drain) => drain.ack,
        }
    }

    /// Spawn `n` mock shard threads that drain a control channel and ack
    /// with the supplied result. Returns the control senders plus the
    /// join handles.
    fn spawn_mock_shards(
        results: Vec<Result<(), String>>,
    ) -> (
        Vec<(WorkerIdx, Sender<ShardCommand>)>,
        Vec<thread::JoinHandle<usize>>,
    ) {
        let mut senders = Vec::new();
        let mut joins = Vec::new();
        for (i, result) in results.into_iter().enumerate() {
            let widx = WorkerIdx(i as u16);
            let (tx, rx) = mpsc::channel::<ShardCommand>();
            senders.push((widx, tx));
            joins.push(thread::spawn(move || {
                let mut applied = 0usize;
                while let Ok(cmd) = rx.recv() {
                    match cmd {
                        ShardCommand::ApplyConfig(apply) => {
                            applied += 1;
                            let _ = apply.ack.send(ShardAck {
                                worker: widx,
                                result: result.clone(),
                            });
                        }
                        ShardCommand::DrainPageCache(drain) => {
                            applied += 1;
                            let _ = drain.ack.send(ShardAck {
                                worker: widx,
                                result: result.clone(),
                            });
                        }
                    }
                }
                applied
            }));
        }
        (senders, joins)
    }

    #[test]
    fn broadcast_apply_blocks_until_every_shard_acks() {
        let (senders, joins) = spawn_mock_shards(vec![Ok(()), Ok(()), Ok(())]);
        let group = control_group(senders);

        let out = group.broadcast_apply(loaded(Config::default()), ConfigDiff::default());
        assert!(out.is_ok(), "expected success, got {out:?}");

        // Closing the group drops the senders so the mock shard threads
        // exit; each must have applied exactly one command.
        drop(group);
        for j in joins {
            assert_eq!(j.join().unwrap(), 1);
        }
    }

    #[test]
    fn broadcast_apply_delivers_the_same_loaded_snapshot() {
        let (tx, rx) = mpsc::channel::<ShardCommand>();
        let group = control_group(vec![(WorkerIdx(0), tx)]);
        let loaded = loaded(Config::default());
        let expected = loaded.clone();
        let join = thread::spawn(move || {
            let ShardCommand::ApplyConfig(apply) = rx.recv().unwrap() else {
                panic!("expected config apply");
            };
            assert!(Arc::ptr_eq(&apply.loaded, &expected));
            apply
                .ack
                .send(ShardAck {
                    worker: WorkerIdx(0),
                    result: Ok(()),
                })
                .unwrap();
        });

        group
            .broadcast_apply(loaded, ConfigDiff::default())
            .unwrap();
        join.join().unwrap();
    }

    #[test]
    fn broadcast_apply_reports_shard_failures() {
        let (senders, joins) = spawn_mock_shards(vec![Ok(()), Err("boom".to_string()), Ok(())]);
        let group = control_group(senders);

        let err = group
            .broadcast_apply(loaded(Config::default()), ConfigDiff::default())
            .expect_err("a failing shard must surface as an error");
        match err {
            ApplyError::ShardApply(failures) => {
                assert_eq!(failures.len(), 1);
                assert_eq!(failures[0].0, WorkerIdx(1));
                assert_eq!(failures[0].1, "boom");
            }
            other => panic!("unexpected error: {other:?}"),
        }

        drop(group);
        for j in joins {
            j.join().unwrap();
        }
    }

    #[test]
    fn broadcast_apply_on_empty_group_is_a_noop() {
        let group = control_group(Vec::new());
        assert!(group.is_empty());
        assert!(
            group
                .broadcast_apply(loaded(Config::default()), ConfigDiff::default(),)
                .is_ok()
        );
    }

    #[test]
    fn broadcast_apply_errors_when_a_shard_channel_is_closed() {
        let (mut senders, joins) = spawn_mock_shards(vec![Ok(())]);
        // Add a dead channel whose receiver has already been dropped.
        let (dead_tx, dead_rx) = mpsc::channel::<ShardCommand>();
        drop(dead_rx);
        senders.push((WorkerIdx(99), dead_tx));
        let group = control_group(senders);

        let err = group
            .broadcast_apply(loaded(Config::default()), ConfigDiff::default())
            .expect_err("closed channel must error");
        assert!(matches!(err, ApplyError::ShardSend(WorkerIdx(99))));
        assert!(err.apply_state_is_indeterminate());

        drop(group);
        for j in joins {
            j.join().unwrap();
        }
    }

    #[test]
    fn reported_shard_failure_is_not_indeterminate() {
        assert!(
            !ApplyError::ShardApply(vec![(WorkerIdx(1), "failed".to_string())])
                .apply_state_is_indeterminate()
        );
        assert!(!ApplyError::Target("rejected".to_string()).apply_state_is_indeterminate());
    }

    #[test]
    fn broadcast_apply_timeout_lists_outstanding_workers() {
        let (tx0, rx0) = mpsc::channel::<ShardCommand>();
        let (tx1, rx1) = mpsc::channel::<ShardCommand>();
        let join0 = thread::spawn(move || {
            let ack = ack_sender(rx0.recv().unwrap());
            ack.send(ShardAck {
                worker: WorkerIdx(0),
                result: Ok(()),
            })
            .unwrap();
        });
        let join1 = thread::spawn(move || {
            let ack = ack_sender(rx1.recv().unwrap());
            thread::sleep(Duration::from_millis(100));
            let _ = ack.send(ShardAck {
                worker: WorkerIdx(1),
                result: Ok(()),
            });
        });
        let group = ShardControlGroup::new(
            vec![(WorkerIdx(0), tx0), (WorkerIdx(1), tx1)],
            Duration::from_millis(20),
        );

        let error = group
            .broadcast_apply(loaded(Config::default()), ConfigDiff::default())
            .expect_err("missing acknowledgement must time out");
        assert!(matches!(
            error,
            ApplyError::AckTimeout {
                expected: 2,
                received: 1,
                outstanding,
            } if outstanding == vec![WorkerIdx(1)]
        ));

        join0.join().unwrap();
        join1.join().unwrap();
    }

    #[test]
    fn duplicate_ack_does_not_hide_outstanding_worker() {
        let (tx0, rx0) = mpsc::channel::<ShardCommand>();
        let (tx1, rx1) = mpsc::channel::<ShardCommand>();
        let join0 = thread::spawn(move || {
            let ack = ack_sender(rx0.recv().unwrap());
            for _ in 0..2 {
                ack.send(ShardAck {
                    worker: WorkerIdx(0),
                    result: Ok(()),
                })
                .unwrap();
            }
        });
        let join1 = thread::spawn(move || {
            let ack = ack_sender(rx1.recv().unwrap());
            thread::sleep(Duration::from_millis(100));
            drop(ack);
        });
        let group = ShardControlGroup::new(
            vec![(WorkerIdx(0), tx0), (WorkerIdx(1), tx1)],
            Duration::from_millis(20),
        );

        let error = group
            .broadcast_drain_page_cache()
            .expect_err("duplicate acknowledgement must not complete fan-in");
        assert!(matches!(
            error,
            ApplyError::AckTimeout {
                expected: 2,
                received: 1,
                outstanding,
            } if outstanding == vec![WorkerIdx(1)]
        ));

        join0.join().unwrap();
        join1.join().unwrap();
    }

    #[test]
    fn disconnected_ack_channel_lists_outstanding_workers() {
        let (tx, rx) = mpsc::channel::<ShardCommand>();
        let join = thread::spawn(move || drop(ack_sender(rx.recv().unwrap())));
        let group = control_group(vec![(WorkerIdx(7), tx)]);

        let error = group
            .broadcast_drain_page_cache()
            .expect_err("dropped acknowledgement sender must disconnect fan-in");
        assert!(matches!(
            error,
            ApplyError::AckDisconnected {
                expected: 1,
                received: 0,
                outstanding,
            } if outstanding == vec![WorkerIdx(7)]
        ));

        join.join().unwrap();
    }

    // ---- ConfigController classification ----

    #[derive(Default)]
    struct RecordingTarget {
        in_place: usize,
        last_diff: Option<ConfigDiff>,
        fail_in_place: bool,
    }

    impl ConfigApplyTarget for RecordingTarget {
        fn apply_in_place(
            &mut self,
            _new: &Arc<LoadedConfig>,
            diff: &ConfigDiff,
        ) -> Result<(), ApplyError> {
            self.last_diff = Some(*diff);
            if self.fail_in_place {
                return Err(ApplyError::ShardApply(vec![(WorkerIdx(0), "nope".into())]));
            }
            self.in_place += 1;
            Ok(())
        }
    }

    fn config_with_peer(version: u64) -> Config {
        let mut c = Config::default();
        c.apply_defaults();
        c.version = version;
        c.self_ = "node-a".to_string();
        c.backends.push(crate::config::BackendSpec {
            name: "b".to_string(),
            config: Some(crate::config::backend_spec::Config::Fake(
                crate::config::FakeBackendConfig {
                    stripe_size_bytes: Some(4 * 1024 * 1024),
                    object_size_bytes: Some(1024 * 1024),
                },
            )),
        });
        c.peers.push(peer("node-a", "127.0.0.1:9999"));
        c
    }

    fn loaded_with_peer(version: u64) -> Arc<LoadedConfig> {
        loaded(config_with_peer(version))
    }

    fn peer(name: &str, discovery_addr: &str) -> crate::config::PeerSpec {
        crate::config::PeerSpec {
            name: name.to_string(),
            tags: Vec::new(),
            discovery_addr: discovery_addr.to_string(),
        }
    }

    #[test]
    fn apply_no_change_runs_neither_tier() {
        let base = loaded_with_peer(1);
        let mut ctrl = ConfigController::new(RecordingTarget::default(), base.clone());
        assert_eq!(
            ctrl.config_versions().snapshot(),
            ConfigVersionSnapshot {
                known: 1,
                applied: 1,
                startup: 1,
            }
        );

        // An apply whose sections are byte-identical to the running config
        // but whose version was bumped takes the NoChange tier yet still
        // advances both known and applied: the process is converged on it.
        let mut bumped = config_with_peer(1);
        bumped.version = 2;
        let out = ctrl.apply(loaded(bumped)).unwrap();
        assert_eq!(out.tier, ApplyTier::NoChange);
        assert_eq!(ctrl.target_mut().in_place, 0);
        assert_eq!(ctrl.config_versions().known(), 2);
        assert_eq!(ctrl.config_versions().applied(), 2);
        // The startup version is pinned to the config realized at
        // construction and must not move when dynamic config advances.
        assert_eq!(ctrl.config_versions().startup(), 1);
    }

    #[test]
    fn apply_no_change_retains_new_raw_policy() {
        let base = loaded_with_peer(1);
        let runtime = base.runtime().disks.clone();
        let mut ctrl = ConfigController::new(RecordingTarget::default(), base);

        let mut next = config_with_peer(2);
        next.disk_discovery = Some(crate::config::DiskDiscovery::default());
        let next = LoadedConfig::from_config(next)
            .unwrap()
            .with_runtime_disks(runtime);
        let out = ctrl.apply(Arc::new(next)).unwrap();

        assert_eq!(out.tier, ApplyTier::NoChange);
        assert!(ctrl.current().config().disk_discovery.is_some());
    }

    #[test]
    fn apply_peer_change_takes_in_place_tier() {
        let base = loaded_with_peer(1);
        let mut ctrl = ConfigController::new(RecordingTarget::default(), base.clone());

        let mut next = config_with_peer(2);
        next.peers.push(peer("node-b", "127.0.0.1:9998"));

        let out = ctrl.apply(loaded(next)).unwrap();
        assert_eq!(out.tier, ApplyTier::InPlace);
        assert_eq!(ctrl.target_mut().in_place, 1);
        assert!(
            ctrl.target_mut()
                .last_diff
                .unwrap()
                .requires_routing_reload()
        );
        assert_eq!(ctrl.config_versions().applied(), 2);
    }

    #[test]
    fn apply_backend_change_takes_in_place_tier() {
        let base = loaded_with_peer(1);
        let mut ctrl = ConfigController::new(RecordingTarget::default(), base.clone());

        let mut next = config_with_peer(3);
        next.backends[0] = crate::config::schema::BackendSpec {
            name: "b".to_string(),
            config: Some(crate::config::backend_spec::Config::Http(
                crate::config::HttpBackendConfig {
                    url: "https://example.com".to_string(),
                    stripe_size_bytes: Some(4 * 1024 * 1024),
                    http_concurrency: Some(64),
                    ca_cert_path: None,
                    insecure_skip_verify: false,
                    client_cert_path: None,
                    client_key_path: None,
                },
            )),
        };

        // A backend change is now reconciled in place on the live shard
        // layer (each shard rebuilds its origin-backend registry from the
        // broadcast config); it no longer rebuilds the shard layer.
        let out = ctrl.apply(loaded(next)).unwrap();
        assert_eq!(out.tier, ApplyTier::InPlace);
        assert_eq!(ctrl.target_mut().in_place, 1);
        assert!(ctrl.target_mut().last_diff.unwrap().backends_changed);
        assert_eq!(ctrl.config_versions().applied(), 3);
    }

    #[test]
    fn failed_apply_leaves_current_config_unchanged() {
        let base = loaded_with_peer(1);
        let target = RecordingTarget {
            fail_in_place: true,
            ..RecordingTarget::default()
        };
        let mut ctrl = ConfigController::new(target, base.clone());

        let mut next = config_with_peer(5);
        next.peers.push(peer("node-b", "127.0.0.1:9998"));
        let next = loaded(next);

        assert!(ctrl.apply(next.clone()).is_err());
        // Current must still be the original so a retry re-derives the
        // same diff.
        assert_eq!(
            ctrl.current().config().peers.len(),
            base.config().peers.len()
        );
        // A failed apply records the version as known (we loaded it) but
        // must NOT advance the applied version: the process did not
        // converge on the submitted config.
        assert_eq!(ctrl.config_versions().known(), 5);
        assert_eq!(ctrl.config_versions().applied(), 1);
    }

    #[test]
    fn config_version_handle_observes_later_applies() {
        let base = loaded_with_peer(1);
        let mut ctrl = ConfigController::new(RecordingTarget::default(), base.clone());

        // A handle taken early must observe applies that happen later,
        // exactly as a future metrics exporter would.
        let versions = ctrl.config_versions();
        assert_eq!(
            versions.snapshot(),
            ConfigVersionSnapshot {
                known: 1,
                applied: 1,
                startup: 1,
            }
        );

        let mut next = config_with_peer(11);
        next.peers.push(peer("node-b", "127.0.0.1:9998"));
        ctrl.apply(loaded(next)).unwrap();

        assert_eq!(versions.known(), 11);
        assert_eq!(versions.applied(), 11);
        assert_eq!(
            versions.snapshot(),
            ConfigVersionSnapshot {
                known: 11,
                applied: 11,
                startup: 1,
            }
        );
    }

    #[test]
    fn startup_version_is_pinned_across_applies_and_failures() {
        let base = loaded_with_peer(1);
        let target = RecordingTarget {
            fail_in_place: true,
            ..RecordingTarget::default()
        };
        let mut ctrl = ConfigController::new(target, base.clone());
        assert_eq!(ctrl.config_versions().startup(), 1);

        // A failed apply advances known but neither applied nor startup.
        let mut failing = config_with_peer(7);
        failing.peers.push(peer("node-b", "127.0.0.1:9998"));
        assert!(ctrl.apply(loaded(failing)).is_err());
        assert_eq!(ctrl.config_versions().known(), 7);
        assert_eq!(ctrl.config_versions().applied(), 1);
        assert_eq!(ctrl.config_versions().startup(), 1);

        // A subsequent successful apply advances known and applied but
        // still leaves startup pinned to the config realized at start.
        ctrl.target_mut().fail_in_place = false;
        let mut next = config_with_peer(8);
        next.peers.push(peer("node-c", "127.0.0.1:9997"));
        ctrl.apply(loaded(next)).unwrap();
        assert_eq!(ctrl.config_versions().applied(), 8);
        assert_eq!(ctrl.config_versions().startup(), 1);
    }
}
