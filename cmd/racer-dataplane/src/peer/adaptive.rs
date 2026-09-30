//! Node-wide outbound admission. Only observed immediate-link failures open circuits.
//! Local overload halves the node limit at most once per 250 ms; verified
//! completions restore one slot per second. Link failures halve that peer's cap
//! and allow one probe after backoff. No queue, background probes, or new byte
//! budget is introduced. Existing worker memory quotas remain authoritative.
use crate::{
    error::{Error, Result},
    model::NodeId,
    telemetry::metrics::{Event, Gauge, Metrics},
};
use std::{
    collections::BTreeMap,
    sync::{Arc, Mutex},
    time::{Duration, Instant},
};

#[derive(Clone, Copy)]
pub struct Config {
    /// Whole-node outstanding exchanges, independent of worker count.
    pub total: usize,
    /// Whole-node outstanding exchanges to one immediate neighbor.
    pub per_peer: usize,
}
impl Default for Config {
    fn default() -> Self {
        Self {
            total: 256,
            per_peer: 32,
        }
    }
}
impl Config {
    pub fn validate(self) -> Result<()> {
        if self.total == 0 || self.total > 65536 || self.per_peer == 0 || self.per_peer > self.total
        {
            return Err(Error::InvalidConfiguration);
        }
        Ok(())
    }
}
const CAPACITY: usize = 256;
const BACKOFF: Duration = Duration::from_millis(250);
const RECOVERY: Duration = Duration::from_secs(1);
pub(crate) struct AdaptivePeers {
    config: Config,
    state: Mutex<State>,
    metrics: Metrics,
}
struct State {
    active: usize,
    limit: usize,
    updated: Instant,
    peers: BTreeMap<NodeId, Peer>,
}
struct Peer {
    active: usize,
    limit: usize,
    generation: u64,
    retry: Option<Instant>,
    probe: bool,
    updated: Instant,
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(crate) enum Outcome {
    Verified,
    PeerFailure,
    LocalPressure,
    Neutral,
}
pub(crate) struct Permit {
    owner: Arc<AdaptivePeers>,
    node: NodeId,
    generation: u64,
    probe: bool,
}
impl AdaptivePeers {
    pub(crate) fn new(config: Config, metrics: Metrics) -> Result<Arc<Self>> {
        config.validate()?;
        metrics.set_gauge(Gauge::PeerAdmissionLimit, config.total as u64);
        Ok(Arc::new(Self {
            config,
            metrics,
            state: Mutex::new(State {
                active: 0,
                limit: config.total,
                updated: crate::runtime::environment::now(),
                peers: BTreeMap::new(),
            }),
        }))
    }
    pub(crate) fn available(&self, node: &NodeId) -> bool {
        let now = crate::runtime::environment::now();
        self.state.lock().is_ok_and(|state| {
            state
                .peers
                .get(node)
                .is_none_or(|p| !p.probe && p.retry.is_none_or(|at| now >= at))
        })
    }
    pub(crate) fn acquire(self: &Arc<Self>, node: &NodeId) -> Result<Arc<Permit>> {
        let now = crate::runtime::environment::now();
        let mut state = self.state.lock().map_err(|_| Error::Unavailable)?;
        if state.active >= state.limit {
            self.metrics.record(Event::PeerAdmissionRejected, 1)?;
            return Err(Error::Overloaded);
        }
        if !state.peers.contains_key(node) && state.peers.len() == CAPACITY {
            // Never discard live work or a circuit still in backoff. Retired entries
            // eventually age out without depending on any worker's retained lease.
            let retired = state
                .peers
                .iter()
                .find(|(_, p)| {
                    p.active == 0
                        && !p.probe
                        && p.retry.is_none_or(|retry| now >= retry)
                        && now.saturating_duration_since(p.updated) >= Duration::from_secs(60)
                })
                .map(|(n, _)| n.clone());
            if let Some(retired) = retired {
                state.peers.remove(&retired);
            } else {
                self.metrics.record(Event::PeerAdmissionRejected, 1)?;
                return Err(Error::Overloaded);
            }
        }
        let peer = state.peers.entry(node.clone()).or_insert(Peer {
            active: 0,
            limit: self.config.per_peer,
            generation: 0,
            retry: None,
            probe: false,
            updated: now,
        });
        if peer.probe || peer.retry.is_some_and(|at| now < at) {
            self.metrics.record(Event::PeerCircuitRejected, 1)?;
            return Err(Error::Unavailable);
        }
        if peer.active >= peer.limit {
            self.metrics.record(Event::PeerAdmissionRejected, 1)?;
            return Err(Error::Overloaded);
        }
        let probe = peer.retry.is_some();
        peer.probe = probe;
        peer.active += 1;
        let generation = peer.generation;
        state.active += 1;
        self.metrics
            .set_gauge(Gauge::PeerExchanges, state.active as u64);
        self.metrics.record(Event::PeerAdmissionAccepted, 1)?;
        if probe {
            self.metrics.record(Event::PeerProbe, 1)?;
        }
        Ok(Arc::new(Permit {
            owner: self.clone(),
            node: node.clone(),
            generation,
            probe,
        }))
    }
}
impl Permit {
    pub(crate) fn observe(&self, outcome: Outcome) {
        let now = crate::runtime::environment::now();
        let Ok(mut state) = self.owner.state.lock() else {
            return;
        };
        // Local receive/connection quota pressure adapts only the node-wide
        // admission limit. It never marks a remote node or link unhealthy.
        if outcome == Outcome::LocalPressure {
            let _ = self.owner.metrics.record(Event::PeerLocalPressure, 1);
            if now.saturating_duration_since(state.updated) >= BACKOFF {
                state.limit = (state.limit / 2).max(1);
                state.updated = now;
                self.owner
                    .metrics
                    .set_gauge(Gauge::PeerAdmissionLimit, state.limit as u64);
            }
            return;
        }
        if outcome == Outcome::Verified && now.saturating_duration_since(state.updated) >= RECOVERY
        {
            state.limit = (state.limit + 1).min(self.owner.config.total);
            state.updated = now;
            self.owner
                .metrics
                .set_gauge(Gauge::PeerAdmissionLimit, state.limit as u64);
        }
        let peer = state
            .peers
            .get_mut(&self.node)
            .expect("live permit retains peer");
        let event = match outcome {
            Outcome::Verified => Event::PeerVerified,
            Outcome::PeerFailure => Event::PeerLinkFailure,
            Outcome::LocalPressure => Event::PeerLocalPressure,
            Outcome::Neutral => return,
        };
        let _ = self.owner.metrics.record(event, 1);
        if peer.generation != self.generation {
            return;
        }
        match outcome {
            Outcome::PeerFailure => {
                peer.limit = (peer.limit / 2).max(1);
                peer.generation = peer.generation.saturating_add(1);
                peer.retry = Some(now + BACKOFF);
                peer.updated = now;
            }
            Outcome::Verified => {
                // Only the exclusive probe can recover an open circuit. Old
                // in-flight successes cannot erase a more recent failure.
                if peer.retry.is_some() && !self.probe {
                    return;
                }
                peer.retry = None;
                if now.saturating_duration_since(peer.updated) >= RECOVERY {
                    peer.limit = (peer.limit + 1).min(self.owner.config.per_peer);
                    peer.updated = now;
                }
            }
            Outcome::LocalPressure | Outcome::Neutral => {}
        }
    }
}
impl Drop for Permit {
    fn drop(&mut self) {
        let Ok(mut state) = self.owner.state.lock() else {
            return;
        };
        let peer = state
            .peers
            .get_mut(&self.node)
            .expect("live permit retains peer");
        peer.active -= 1;
        if self.probe {
            peer.probe = false;
            if peer.retry.is_some() {
                let now = crate::runtime::environment::now();
                peer.retry = Some(now + BACKOFF);
                peer.updated = now;
            }
        }
        state.active -= 1;
        self.owner
            .metrics
            .set_gauge(Gauge::PeerExchanges, state.active as u64);
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn churn_cannot_evict_neutral_probe_backoff_with_old_update_time() {
        let clock = crate::runtime::environment::SimulationClock::new(783);
        let _env = clock.environment(0).enter();
        let owner = AdaptivePeers::new(
            Config {
                total: 512,
                per_peer: 1,
            },
            Metrics::default(),
        )
        .unwrap();
        let node = NodeId("000-circuit".into());
        let permit = owner.acquire(&node).unwrap();
        permit.observe(Outcome::PeerFailure);
        drop(permit);
        let held: Vec<_> = (1..CAPACITY)
            .map(|i| owner.acquire(&NodeId(i.to_string())).unwrap())
            .collect();
        clock.advance(Duration::from_secs(61));
        let probe = owner.acquire(&node).unwrap();
        drop(probe);
        assert!(!owner.available(&node));
        assert!(matches!(
            owner.acquire(&NodeId("new".into())),
            Err(Error::Overloaded)
        ));
        // Even a stale timestamp must not make an active backoff evictable.
        owner
            .state
            .lock()
            .unwrap()
            .peers
            .get_mut(&node)
            .unwrap()
            .updated = crate::runtime::environment::now() - Duration::from_secs(61);
        assert!(matches!(
            owner.acquire(&NodeId("new".into())),
            Err(Error::Overloaded)
        ));
        assert!(owner.state.lock().unwrap().peers.contains_key(&node));
        drop(held);
    }
    #[test]
    fn shared_caps_local_pressure_and_completion_fences() {
        let metrics = Metrics::default();
        let owner = AdaptivePeers::new(
            Config {
                total: 2,
                per_peer: 1,
            },
            metrics.clone(),
        )
        .unwrap();
        let worker = owner.clone();
        let a = NodeId("a".into());
        let b = NodeId("b".into());
        let permit = owner.acquire(&a).unwrap();
        assert!(matches!(worker.acquire(&a), Err(Error::Overloaded)));
        let io = permit.clone();
        permit.observe(Outcome::LocalPressure);
        drop(permit);
        assert!(matches!(worker.acquire(&a), Err(Error::Overloaded)));
        let second = worker.acquire(&b).unwrap();
        assert!(matches!(
            owner.acquire(&NodeId("c".into())),
            Err(Error::Overloaded)
        ));
        assert_eq!(metrics.gauge(Gauge::PeerExchanges), 2);
        drop((io, second));
        assert_eq!(metrics.gauge(Gauge::PeerExchanges), 0);
        assert!(owner.available(&a));
        assert_eq!(metrics.count(Event::PeerLocalPressure), 1);
        assert_eq!(metrics.count(Event::PeerLinkFailure), 0);
    }
    #[test]
    fn recovery_requires_exclusive_verified_probe_and_old_success_cannot_clear_failure() {
        let clock = crate::runtime::environment::SimulationClock::new(779);
        let _env = clock.environment(0).enter();
        let owner = AdaptivePeers::new(
            Config {
                total: 4,
                per_peer: 4,
            },
            Metrics::default(),
        )
        .unwrap();
        let node = NodeId("a".into());
        let old = owner.acquire(&node).unwrap();
        let failed = owner.acquire(&node).unwrap();
        failed.observe(Outcome::PeerFailure);
        old.observe(Outcome::Verified);
        assert!(!owner.available(&node));
        drop((old, failed));
        clock.advance(BACKOFF);
        let probe = owner.acquire(&node).unwrap();
        assert!(matches!(owner.acquire(&node), Err(Error::Unavailable)));
        assert!(!owner.available(&node));
        drop(probe); // Cancellation/unverified completion cannot recover.
        assert!(!owner.available(&node));
        clock.advance(RECOVERY);
        let probe = owner.acquire(&node).unwrap();
        probe.observe(Outcome::Verified);
        assert!(!owner.available(&node)); // Still exclusive until its I/O fence.
        drop(probe);
        assert!(owner.available(&node));
        let state = owner.state.lock().unwrap();
        assert_eq!(state.peers[&node].limit, 3);
    }
    #[test]
    fn state_capacity_never_evicts_live_permits_and_ages_retired_entries() {
        let clock = crate::runtime::environment::SimulationClock::new(780);
        let _env = clock.environment(0).enter();
        let owner = AdaptivePeers::new(
            Config {
                total: 512,
                per_peer: 1,
            },
            Metrics::default(),
        )
        .unwrap();
        let permits: Vec<_> = (0..CAPACITY)
            .map(|i| owner.acquire(&NodeId(i.to_string())).unwrap())
            .collect();
        clock.advance(Duration::from_secs(61));
        assert!(matches!(
            owner.acquire(&NodeId("new".into())),
            Err(Error::Overloaded)
        ));
        drop(permits);
        let _permit = owner.acquire(&NodeId("new".into())).unwrap();
        assert_eq!(owner.state.lock().unwrap().peers.len(), CAPACITY);
    }

    #[test]
    fn local_pressure_shrinks_node_limit_without_revoking_work_or_blame() {
        let clock = crate::runtime::environment::SimulationClock::new(781);
        let _env = clock.environment(0).enter();
        let metrics = Metrics::default();
        let owner = AdaptivePeers::new(
            Config {
                total: 4,
                per_peer: 4,
            },
            metrics.clone(),
        )
        .unwrap();
        let node = NodeId("a".into());
        let permits: Vec<_> = (0..4).map(|_| owner.acquire(&node).unwrap()).collect();
        clock.advance(BACKOFF);
        for permit in &permits {
            permit.observe(Outcome::LocalPressure);
        }
        assert_eq!(metrics.gauge(Gauge::PeerAdmissionLimit), 2);
        assert_eq!(metrics.gauge(Gauge::PeerExchanges), 4);
        assert!(owner.available(&node));
        assert_eq!(metrics.count(Event::PeerLinkFailure), 0);
        assert!(matches!(
            owner.acquire(&NodeId("other".into())),
            Err(Error::Overloaded)
        ));
        clock.advance(RECOVERY);
        permits[0].observe(Outcome::Verified);
        assert_eq!(metrics.gauge(Gauge::PeerAdmissionLimit), 3);
        drop(permits);
        assert_eq!(metrics.gauge(Gauge::PeerExchanges), 0);
    }
}
