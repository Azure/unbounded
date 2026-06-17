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
//! republished through the shared [`RoutingHandle`], and each shard
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

use std::fmt;
use std::sync::Arc;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::mpsc::{self, RecvError, Sender};

use crate::config::{Config, ConfigDiff};
use crate::p2p::RouteTableSnapshot;
use crate::runtime::WorkerIdx;

/// A command delivered to a single shard's control channel. The shard
/// drains its receiver from a tick hook on its own thread, applies the
/// command there (so all `!Send` per-shard state stays thread-local),
/// then acknowledges.
pub enum ShardCommand {
    /// Apply a new configuration in place: republish routing and
    /// reconcile this shard's backend and frontend registries.
    ApplyConfig(ShardApply),
}

/// Payload of [`ShardCommand::ApplyConfig`].
pub struct ShardApply {
    /// The new, defaults-applied, validated configuration.
    pub config: Arc<Config>,
    /// The route table rebuilt from `config`. Published into the shard's
    /// route-table handle so transports observe it atomically.
    pub routes: RouteTableSnapshot,
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
    AckDisconnected { expected: usize, received: usize },
    /// One or more shards reported a failure while applying.
    ShardApply(Vec<(WorkerIdx, String)>),
    /// The process-level apply target rejected the config before shard
    /// broadcast.
    Target(String),
}

impl fmt::Display for ApplyError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            ApplyError::ShardSend(w) => {
                write!(f, "shard {} control channel closed before apply", w.0)
            }
            ApplyError::AckDisconnected { expected, received } => write!(
                f,
                "ack channel disconnected after {received}/{expected} shards reported",
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
    fn apply_in_place(&mut self, new: &Arc<Config>, diff: &ConfigDiff) -> Result<(), ApplyError>;
}

/// The single funnel for configuration changes. Holds the currently
/// applied config and delegates the process-specific work to a
/// [`ConfigApplyTarget`].
pub struct ConfigController<T: ConfigApplyTarget> {
    target: T,
    current: Arc<Config>,
    versions: ConfigVersionStatus,
}

impl<T: ConfigApplyTarget> ConfigController<T> {
    /// Create a controller seeded with the configuration already applied
    /// to the process at startup. The latest-known, latest-applied, and
    /// startup config versions are all seeded from `initial`'s
    /// [`Config::version`].
    pub fn new(target: T, initial: Arc<Config>) -> Self {
        let versions = ConfigVersionStatus::new(initial.version);
        Self {
            target,
            current: initial,
            versions,
        }
    }

    /// The configuration currently realized by the process.
    pub fn current(&self) -> &Arc<Config> {
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
    pub fn apply(&mut self, new: Arc<Config>) -> Result<ApplyOutcome, ApplyError> {
        // Record the loaded version before doing any work so the
        // latest-known version advances even if the apply fails below.
        self.versions.record_known(new.version);

        let diff = ConfigDiff::between(&self.current, &new);

        if !diff.any() {
            self.versions.record_applied(new.version);
            return Ok(ApplyOutcome {
                tier: ApplyTier::NoChange,
                diff,
            });
        }

        self.target.apply_in_place(&new, &diff)?;

        self.versions.record_applied(new.version);
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
/// has acknowledged, so the caller can guarantee the change has landed
/// on every shard thread before returning. This is the concrete
/// "close the loop" mechanism a [`ConfigApplyTarget`] builds its Tier 1
/// path on.
pub struct ShardControlGroup {
    senders: Vec<(WorkerIdx, Sender<ShardCommand>)>,
}

impl ShardControlGroup {
    pub fn new(senders: Vec<(WorkerIdx, Sender<ShardCommand>)>) -> Self {
        Self { senders }
    }

    pub fn len(&self) -> usize {
        self.senders.len()
    }

    pub fn is_empty(&self) -> bool {
        self.senders.is_empty()
    }

    /// Send `config` + `routes` to every shard and block until all of
    /// them acknowledge.
    ///
    /// Returns `Ok(())` only when every shard reports success. If a
    /// channel is closed at send time, or the ack channel disconnects
    /// before all shards report, or any shard reports a failure, the
    /// corresponding [`ApplyError`] is returned. The fan-in always
    /// drains every ack that does arrive so a single failing shard does
    /// not mask the others.
    pub fn broadcast_apply(
        &self,
        config: Arc<Config>,
        routes: RouteTableSnapshot,
    ) -> Result<(), ApplyError> {
        let expected = self.senders.len();
        if expected == 0 {
            return Ok(());
        }

        let (ack_tx, ack_rx) = mpsc::channel::<ShardAck>();

        for (worker, sender) in &self.senders {
            let cmd = ShardCommand::ApplyConfig(ShardApply {
                config: config.clone(),
                routes: routes.clone(),
                ack: ack_tx.clone(),
            });
            sender
                .send(cmd)
                .map_err(|_| ApplyError::ShardSend(*worker))?;
        }
        // Drop our own handle so the channel closes once every shard's
        // cloned sender is dropped; that is how the fan-in loop below
        // detects "no more acks coming".
        drop(ack_tx);

        let mut received = 0usize;
        let mut failures = Vec::new();
        loop {
            match ack_rx.recv() {
                Ok(ack) => {
                    received += 1;
                    if let Err(e) = ack.result {
                        failures.push((ack.worker, e));
                    }
                    if received == expected {
                        break;
                    }
                }
                Err(RecvError) => {
                    return Err(ApplyError::AckDisconnected { expected, received });
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

#[cfg(test)]
mod tests {
    use std::sync::Arc;
    use std::sync::mpsc;
    use std::thread;

    use super::*;
    use crate::p2p::RouteTableSnapshot;

    fn empty_routes() -> RouteTableSnapshot {
        RouteTableSnapshot::default()
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
        let group = ShardControlGroup::new(senders);

        let out = group.broadcast_apply(Arc::new(Config::default()), empty_routes());
        assert!(out.is_ok(), "expected success, got {out:?}");

        // Closing the group drops the senders so the mock shard threads
        // exit; each must have applied exactly one command.
        drop(group);
        for j in joins {
            assert_eq!(j.join().unwrap(), 1);
        }
    }

    #[test]
    fn broadcast_apply_reports_shard_failures() {
        let (senders, joins) = spawn_mock_shards(vec![Ok(()), Err("boom".to_string()), Ok(())]);
        let group = ShardControlGroup::new(senders);

        let err = group
            .broadcast_apply(Arc::new(Config::default()), empty_routes())
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
        let group = ShardControlGroup::new(Vec::new());
        assert!(group.is_empty());
        assert!(
            group
                .broadcast_apply(Arc::new(Config::default()), empty_routes())
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
        let group = ShardControlGroup::new(senders);

        let err = group
            .broadcast_apply(Arc::new(Config::default()), empty_routes())
            .expect_err("closed channel must error");
        assert!(matches!(err, ApplyError::ShardSend(WorkerIdx(99))));

        drop(group);
        for j in joins {
            j.join().unwrap();
        }
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
            _new: &Arc<Config>,
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
        c.backends.push(crate::config::BackendSpec {
            id: "b".to_string(),
            config: Some(crate::config::backend_spec::Config::Fake(
                crate::config::FakeBackendConfig {
                    stripe_size_bytes: 4 * 1024 * 1024,
                    object_size_bytes: 1024 * 1024,
                },
            )),
        });
        c.neighborhoods.push(crate::config::NeighborhoodSpec {
            id: "n".to_string(),
            binds_to: "b".to_string(),
            fingers_per_node: 100,
            local_node_id: Some(0),
            local_labels: Vec::new(),
            routing_plan: None,
            peers: vec![tcp_peer(1, "127.0.0.1:9999")],
        });
        c
    }

    fn tcp_peer(id: u64, addr: &str) -> crate::config::PeerSpec {
        crate::config::PeerSpec {
            id,
            labels: Vec::new(),
            config: Some(crate::config::peer_spec::Config::Tcp(
                crate::config::TcpPeerConfig {
                    addr: addr.to_string(),
                },
            )),
        }
    }

    #[test]
    fn apply_no_change_runs_neither_tier() {
        let base = Arc::new(config_with_peer(1));
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
        let out = ctrl.apply(Arc::new(bumped)).unwrap();
        assert_eq!(out.tier, ApplyTier::NoChange);
        assert_eq!(ctrl.target_mut().in_place, 0);
        assert_eq!(ctrl.config_versions().known(), 2);
        assert_eq!(ctrl.config_versions().applied(), 2);
        // The startup version is pinned to the config realized at
        // construction and must not move when dynamic config advances.
        assert_eq!(ctrl.config_versions().startup(), 1);
    }

    #[test]
    fn apply_peer_change_takes_in_place_tier() {
        let base = Arc::new(config_with_peer(1));
        let mut ctrl = ConfigController::new(RecordingTarget::default(), base.clone());

        let mut next = config_with_peer(2);
        next.neighborhoods[0]
            .peers
            .push(tcp_peer(2, "127.0.0.1:9998"));

        let out = ctrl.apply(Arc::new(next)).unwrap();
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
        let base = Arc::new(config_with_peer(1));
        let mut ctrl = ConfigController::new(RecordingTarget::default(), base.clone());

        let mut next = config_with_peer(3);
        next.backends.push(crate::config::schema::BackendSpec {
            id: "b".to_string(),
            config: Some(crate::config::backend_spec::Config::Http(
                crate::config::HttpBackendConfig {
                    endpoint: "https://example.com".to_string(),
                    stripe_size_bytes: 4 * 1024 * 1024,
                    http_concurrency: 64,
                },
            )),
        });

        // A backend change is now reconciled in place on the live shard
        // layer (each shard rebuilds its origin-backend registry from the
        // broadcast config); it no longer rebuilds the shard layer.
        let out = ctrl.apply(Arc::new(next)).unwrap();
        assert_eq!(out.tier, ApplyTier::InPlace);
        assert_eq!(ctrl.target_mut().in_place, 1);
        assert!(ctrl.target_mut().last_diff.unwrap().backends_changed);
        assert_eq!(ctrl.config_versions().applied(), 3);
    }

    #[test]
    fn failed_apply_leaves_current_config_unchanged() {
        let base = Arc::new(config_with_peer(1));
        let target = RecordingTarget {
            fail_in_place: true,
            ..RecordingTarget::default()
        };
        let mut ctrl = ConfigController::new(target, base.clone());

        let mut next = config_with_peer(5);
        next.neighborhoods[0]
            .peers
            .push(tcp_peer(2, "127.0.0.1:9998"));
        let next = Arc::new(next);

        assert!(ctrl.apply(next.clone()).is_err());
        // Current must still be the original so a retry re-derives the
        // same diff.
        assert_eq!(
            ctrl.current().neighborhoods[0].peers.len(),
            base.neighborhoods[0].peers.len()
        );
        // A failed apply records the version as known (we loaded it) but
        // must NOT advance the applied version: the process did not
        // converge on the submitted config.
        assert_eq!(ctrl.config_versions().known(), 5);
        assert_eq!(ctrl.config_versions().applied(), 1);
    }

    #[test]
    fn config_version_handle_observes_later_applies() {
        let base = Arc::new(config_with_peer(1));
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
        next.neighborhoods[0]
            .peers
            .push(tcp_peer(2, "127.0.0.1:9998"));
        ctrl.apply(Arc::new(next)).unwrap();

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
        let base = Arc::new(config_with_peer(1));
        let target = RecordingTarget {
            fail_in_place: true,
            ..RecordingTarget::default()
        };
        let mut ctrl = ConfigController::new(target, base.clone());
        assert_eq!(ctrl.config_versions().startup(), 1);

        // A failed apply advances known but neither applied nor startup.
        let mut failing = config_with_peer(7);
        failing.neighborhoods[0]
            .peers
            .push(tcp_peer(2, "127.0.0.1:9998"));
        assert!(ctrl.apply(Arc::new(failing)).is_err());
        assert_eq!(ctrl.config_versions().known(), 7);
        assert_eq!(ctrl.config_versions().applied(), 1);
        assert_eq!(ctrl.config_versions().startup(), 1);

        // A subsequent successful apply advances known and applied but
        // still leaves startup pinned to the config realized at start.
        ctrl.target_mut().fail_in_place = false;
        let mut next = config_with_peer(8);
        next.neighborhoods[0]
            .peers
            .push(tcp_peer(3, "127.0.0.1:9997"));
        ctrl.apply(Arc::new(next)).unwrap();
        assert_eq!(ctrl.config_versions().applied(), 8);
        assert_eq!(ctrl.config_versions().startup(), 1);
    }
}
