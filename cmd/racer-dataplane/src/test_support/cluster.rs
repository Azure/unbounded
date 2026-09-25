//! Virtual topology/control fixtures and side-effect-free composition configuration.
use super::clock::{Clock, Schedule};
use crate::{
    config::Config,
    error::{Error, Result},
    model::{
        identity::{ClusterId, NodeId},
        limits::Limits,
    },
};
use std::{
    cell::RefCell,
    collections::{BTreeMap, BTreeSet},
    num::NonZeroUsize,
    time::Duration,
};

#[derive(Clone, Debug)]
pub enum LinkFault {
    Partition(NodeId, NodeId),
    Heal(NodeId, NodeId),
    Ready(NodeId, bool),
}

/// Transport reachability, deliberately separate from accepted membership/placement.
/// A partition never removes a node or changes its weight/identity.
pub struct Cluster {
    nodes: RefCell<BTreeMap<NodeId, bool>>,
    partitions: RefCell<BTreeSet<(NodeId, NodeId)>>,
    faults: RefCell<Schedule<LinkFault>>,
}

impl Default for Cluster {
    fn default() -> Self {
        Self::new(Clock::default())
    }
}

impl Cluster {
    pub fn new(clock: Clock) -> Self {
        Self {
            nodes: RefCell::new(BTreeMap::new()),
            partitions: RefCell::new(BTreeSet::new()),
            faults: RefCell::new(Schedule::new(clock)),
        }
    }

    pub fn add(&self, node: NodeId) -> Result<()> {
        let mut nodes = self.nodes.borrow_mut();
        if nodes.contains_key(&node) {
            return Err(Error::InvalidRequest);
        }
        nodes.insert(node, true);
        Ok(())
    }

    pub fn nodes(&self) -> Vec<NodeId> {
        self.nodes.borrow().keys().cloned().collect()
    }

    fn pair(&self, left: &NodeId, right: &NodeId) -> Result<(NodeId, NodeId)> {
        let nodes = self.nodes.borrow();
        if left == right || !nodes.contains_key(left) || !nodes.contains_key(right) {
            return Err(Error::InvalidRequest);
        }
        Ok(if left < right {
            (left.clone(), right.clone())
        } else {
            (right.clone(), left.clone())
        })
    }

    pub fn partition(&self, left: &NodeId, right: &NodeId) -> Result<()> {
        self.partitions.borrow_mut().insert(self.pair(left, right)?);
        Ok(())
    }

    pub fn heal(&self, left: &NodeId, right: &NodeId) -> Result<()> {
        self.partitions
            .borrow_mut()
            .remove(&self.pair(left, right)?);
        Ok(())
    }

    pub fn set_ready(&self, node: &NodeId, ready: bool) -> Result<()> {
        *self
            .nodes
            .borrow_mut()
            .get_mut(node)
            .ok_or(Error::InvalidRequest)? = ready;
        Ok(())
    }

    /// A link probe, not a route search: no implicit relay or membership mutation.
    pub fn connect(&self, left: &NodeId, right: &NodeId) -> Result<()> {
        let pair = self.pair(left, right)?;
        let nodes = self.nodes.borrow();
        if !nodes[left] || !nodes[right] || self.partitions.borrow().contains(&pair) {
            Err(Error::Unavailable)
        } else {
            Ok(())
        }
    }

    pub fn schedule(&self, at: Duration, fault: LinkFault) -> Result<()> {
        match &fault {
            LinkFault::Partition(left, right) | LinkFault::Heal(left, right) => {
                self.pair(left, right)?;
            }
            LinkFault::Ready(node, _) => {
                if !self.nodes.borrow().contains_key(node) {
                    return Err(Error::InvalidRequest);
                }
            }
        }
        self.faults.borrow_mut().push(at, fault)
    }

    pub fn poll_budgeted(&self, budget: usize) -> Result<usize> {
        let mut applied = 0;
        while applied < budget {
            let fault = self.faults.borrow_mut().pop_ready();
            let Some(fault) = fault else { break };
            match fault {
                LinkFault::Partition(left, right) => self.partition(&left, &right)?,
                LinkFault::Heal(left, right) => self.heal(&left, &right)?,
                LinkFault::Ready(node, ready) => self.set_ready(&node, ready)?,
            }
            applied += 1;
        }
        Ok(applied)
    }
}
pub fn config(enable_rdma: bool) -> Config {
    let count = NonZeroUsize::new(16).unwrap();
    let bytes = NonZeroUsize::new(128 * 1024 * 1024).unwrap();
    Config {
        cluster: ClusterId("00000000-0000-4000-8000-000000000001".into()),
        node: NodeId("00000000-0000-4000-8000-000000000002".into()),
        max_threads: 2,
        enable_rdma,
        control_endpoint: "https://control.invalid".into(),
        peer_listen: "127.0.0.1:0".parse().unwrap(),
        diagnostics_listen: "127.0.0.1:0".parse().unwrap(),
        trust_bundle: "unused/ca".into(),
        service_account_token: "unused/token".into(),
        secret_directory: "unused/secrets".into(),
        identity_directory: "unused/identity".into(),
        slab_directory: "unused/slabs".into(),
        slab_bytes: 1024 * 1024 * 1024,
        segment_bytes: 64 * 1024 * 1024,
        free_segment_reserve: 2,
        request_timeout: Duration::from_secs(30),
        reader_stall_timeout: Duration::from_secs(10),
        shutdown_timeout: Duration::from_secs(30),
        limits: Limits {
            plaintext_bytes: bytes,
            ciphertext_bytes: bytes,
            dirty_bytes: bytes,
            registered_bytes: bytes,
            request_context_bytes: bytes,
            flights: count,
            waiters_per_flight: count,
            queue_entries: count,
            connections_per_neighbor: count,
            client_connections: count,
            pipes: count,
            range_window_pages: count,
            replay_entries: count,
            header_bytes: NonZeroUsize::new(16 * 1024).unwrap(),
            route_search_work: count,
            cached_rankings: count,
            cached_paths: count,
            retained_snapshots: count,
            metadata_entries: count,
            relay_transfers: count,
        },
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn partitions_are_symmetric_and_readiness_never_changes_membership() {
        let cluster = Cluster::default();
        let [a, b, c] = ["a", "b", "c"].map(|name| NodeId(name.into()));
        for node in [&c, &a, &b] {
            cluster.add(node.clone()).unwrap();
        }
        let membership = cluster.nodes();
        assert_eq!(membership, vec![a.clone(), b.clone(), c.clone()]);
        cluster.partition(&a, &b).unwrap();
        assert_eq!(cluster.connect(&a, &b), Err(Error::Unavailable));
        assert_eq!(cluster.connect(&b, &a), Err(Error::Unavailable));
        assert_eq!(cluster.connect(&a, &c), Ok(()));
        cluster.set_ready(&b, false).unwrap();
        cluster.heal(&b, &a).unwrap();
        assert_eq!(cluster.connect(&a, &b), Err(Error::Unavailable));
        assert_eq!(cluster.nodes(), membership);
        assert_eq!(cluster.add(b.clone()), Err(Error::InvalidRequest));
        assert_eq!(cluster.connect(&b, &c), Err(Error::Unavailable));
        cluster.set_ready(&b, true).unwrap();
        assert_eq!(cluster.connect(&a, &b), Ok(()));
        assert_eq!(cluster.partition(&a, &a), Err(Error::InvalidRequest));
        assert_eq!(
            cluster.partition(&a, &NodeId("missing".into())),
            Err(Error::InvalidRequest)
        );
    }

    #[test]
    fn scheduled_partition_heal_and_restart_are_budgeted_and_ordered() {
        let clock = Clock::default();
        let cluster = Cluster::new(clock.clone());
        let a = NodeId("a".into());
        let b = NodeId("b".into());
        cluster.add(a.clone()).unwrap();
        cluster.add(b.clone()).unwrap();
        for fault in [
            LinkFault::Partition(a.clone(), b.clone()),
            LinkFault::Heal(a.clone(), b.clone()),
            LinkFault::Ready(b.clone(), false),
            LinkFault::Ready(b.clone(), true),
        ] {
            cluster.schedule(Duration::from_secs(1), fault).unwrap();
        }
        assert_eq!(cluster.poll_budgeted(8), Ok(0));
        clock.advance(Duration::from_secs(1)).unwrap();
        assert_eq!(cluster.poll_budgeted(0), Ok(0));
        for expected in [
            Err(Error::Unavailable),
            Ok(()),
            Err(Error::Unavailable),
            Ok(()),
        ] {
            assert_eq!(cluster.poll_budgeted(1), Ok(1));
            assert_eq!(cluster.connect(&a, &b), expected);
        }
        assert_eq!(cluster.poll_budgeted(8), Ok(0));
    }
}
