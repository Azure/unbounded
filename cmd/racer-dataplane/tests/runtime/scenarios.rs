// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Borrowed observers keep generation/session assertions readable without
// widening runtime's production API or cloning mutable manager state.
fn manager(generation: &Generation) -> std::cell::Ref<'_, Manager> {
    generation.manager.as_ref().unwrap().borrow()
}
fn manager_mut(generation: &Generation) -> std::cell::RefMut<'_, Manager> {
    generation.manager.as_ref().unwrap().borrow_mut()
}

fn generation(volumes: &Volumes, address: SocketAddr) -> Rc<Generation> {
    volumes.servers[&local_key(address)]
        .handler()
        .current
        .clone()
}
fn local_key(address: SocketAddr) -> Address {
    Address::unix(&crate::control::tests::test_socket(address, "cache")).unwrap()
}

// Included in runtime::tests. Real TCP/io_uring, independent node caches/pools.
#[path = "membership.rs"]
mod membership;

pub(crate) struct Cluster {
    rings: Vec<uring::Ring>,
    nodes: Vec<Option<Volumes>>,
    addresses: Vec<SocketAddr>,
    credentials: Vec<Arc<crate::control::credentials::Provider>>,
    pub(crate) hits: Arc<std::sync::Mutex<Vec<(usize, String)>>>,
    stop: Arc<std::sync::atomic::AtomicBool>,
    backends: Vec<std::thread::JoinHandle<()>>,
}
impl Cluster {
    pub(crate) fn config(&self, node: usize) -> Arc<Prepared> {
        self.generation(node)._config.clone()
    }
    pub(crate) fn reload(&mut self, node: usize, algorithm: Option<u32>) {
        let old = self.generation(node);
        let (mut trust, _) = fixture();
        trust.node = [node as u8 + 10; 32];
        let mut config = old._config.config_snapshot().clone();
        config.volumes[0].origin_socket = old._config.config_snapshot().volumes[0]
            .origin_socket
            .clone();
        config.revision = 2;
        let topology = config.volumes[0].topology.as_mut().unwrap();
        topology.epoch = 2;
        topology.routing_algorithm = algorithm;
        self.nodes[node]
            .as_ref()
            .unwrap()
            .updates
            .publish(crate::control::tests::prepare_cluster_snapshot(
                &trust, config,
            ))
            .unwrap();
        self.turn();
        assert!(!old.active.get());
        assert!(
            old.manager.as_ref().is_none_or(|m| m
                .borrow()
                .live
                .iter()
                .all(|p| p.connection.is_healthy()))
        );
    }
    pub(crate) fn expire_previous(&mut self, node: usize) {
        let server = &self.nodes[node].as_ref().unwrap().servers[&self.local_address(node)];
        for old in &server.handler().draining {
            old.drain.set(Some(Instant::now()));
        }
        self.turn();
    }
    fn generation(&self, node: usize) -> Rc<Generation> {
        generation(self.nodes[node].as_ref().unwrap(), self.addresses[node])
    }
    fn local_address(&self, node: usize) -> crate::socket::Address {
        crate::socket::Address::unix(&crate::control::tests::test_socket(
            self.addresses[node],
            "cache",
        ))
        .unwrap()
    }
    pub(crate) fn new() -> Option<Self> {
        // Match the bounded daemon pool while leaving room for downstream staging.
        drop(ring()?);
        let new_ring = || crate::conformance::ring(8, uring::Config::default());
        let first = new_ring();
        let reservations: Vec<_> = (0..8)
            .map(|_| std::net::TcpListener::bind(address()).unwrap())
            .collect();
        let addresses: Vec<_> = reservations
            .iter()
            .map(|l| l.local_addr().unwrap())
            .collect();
        drop(reservations);
        let hits = Arc::new(std::sync::Mutex::new(Vec::new()));
        let stop = Arc::new(std::sync::atomic::AtomicBool::new(false));
        let mut cluster = Self {
            rings: vec![first],
            nodes: vec![],
            addresses,
            credentials: vec![],
            hits,
            stop,
            backends: vec![],
        };
        for node in 0..8 {
            if node > 0 {
                cluster.rings.push(new_ring());
            }
            let (backend, task) =
                crate::conformance::origin(node, cluster.stop.clone(), cluster.hits.clone());
            cluster.backends.push(task);
            let (mut trust, _) = fixture();
            trust.node = [node as u8 + 10; 32];
            let mut config =
                cluster_config(node, &cluster.addresses, backend, Some(1), "topology-test");
            peer_endpoints(&mut config);
            let prepared = crate::control::tests::prepare_cluster_snapshot(&trust, config);
            let updates = Arc::new(Updates::default());
            let provider = tls_provider(&trust.universe, &trust.node, &format!("pod-{node}"));
            updates.set_credentials(provider.clone());
            cluster.credentials.push(provider);
            updates.subscribe(cluster.rings[node].wake_handle());
            updates.publish(prepared).unwrap();
            let crypto = Arc::new(crate::crypto::Pool::test_pool(cluster.rings[node].pool()));
            let worker = 0;
            let mut volumes = Volumes::new(crate::cache::tests::cache(1), updates, crypto, worker)
                .with_peer_ip(cluster.addresses[node].ip());
            volumes.poll(&mut cluster.rings[node], 64).unwrap();
            cluster.nodes.push(Some(volumes));
        }
        Some(cluster)
    }

    fn turn(&mut self) {
        for (ring, node) in self.rings.iter_mut().zip(&mut self.nodes) {
            ring.progress().unwrap();
            if let Some(node) = node {
                node.poll(ring, 64).unwrap();
            }
        }
    }
    pub(crate) fn target(&self, owner: u32, prefix: &str) -> String {
        let config = self.config(0);
        let routing = config.volumes()[0].routing();
        (0..)
            .map(|n| format!("/{prefix}?exact=%2f&v={n}"))
            .find(|t| routing.start(t).owner == owner)
            .unwrap()
    }
    pub(crate) fn get(&mut self, node: usize, target: &str) -> (u16, Vec<u8>) {
        self.get_headers(node, target, &[])
    }
    pub(crate) fn get_headers(
        &mut self,
        node: usize,
        target: &str,
        headers: &[(&str, &str)],
    ) -> (u16, Vec<u8>) {
        use crate::{buffers::Key, http_client as client};
        let key =
            *blake3::hash(format!("response {node} {target} {headers:?}").as_bytes()).as_bytes();
        let fill = self.rings[node].pool().stage(Key::new(key)).unwrap();
        let end = Instant::now() + Duration::from_secs(15);
        let mut peer_headers: Vec<_> = headers
            .iter()
            .map(|(n, v)| (n.to_string(), v.as_bytes().to_vec()))
            .collect();
        let peer = headers
            .iter()
            .any(|(n, _)| n.eq_ignore_ascii_case("x-racer-fault"));
        if peer {
            if !headers
                .iter()
                .any(|(n, _)| n.eq_ignore_ascii_case("x-racer-attempt"))
            {
                let wire = headers
                    .iter()
                    .find(|(n, _)| n.eq_ignore_ascii_case("x-racer-fault"))
                    .unwrap()
                    .1;
                let bytes = crate::cache::peer_wire::unhex(wire).unwrap();
                peer_headers.push((
                    "X-Racer-Attempt".into(),
                    format!(
                        "{}{}",
                        crate::cache::peer_wire::hex(
                            crate::authorization::binding(&bytes, &Default::default()).as_bytes()
                        ),
                        "a".repeat(32)
                    )
                    .into_bytes(),
                ));
            }
            peer_headers.push((
                "X-Racer-Volume".into(),
                self.config(node).volumes()[0]
                    .config()
                    .id
                    .as_bytes()
                    .to_vec(),
            ));
        }
        let headers: Vec<_> = peer_headers
            .iter()
            .map(|(n, v)| (n.as_str(), std::str::from_utf8(v).unwrap()))
            .collect();
        let connection = if peer {
            client::Connection::new_tls(
                peer_address(self.addresses[node]),
                "localhost",
                &self.credentials[0].current().context,
                crate::tls::ExpectedPeer::Identity(self.credentials[node].identity().clone()),
            )
        } else {
            client::Connection::new_address(self.local_address(node), "localhost")
        }
        .unwrap();
        let mut request = connection
            .get(client::Request::new(target, &headers).unwrap(), fill, end)
            .unwrap();
        loop {
            self.turn();
            if let Progress::Ready(mut response) = request.poll(&mut self.rings[node], 64).unwrap()
            {
                return (response.status(), response.body().to_vec());
            }
            assert!(Instant::now() < end, "topology request stalled");
        }
    }
    pub(crate) fn remove(&mut self, node: usize) {
        if let Some(mut volume) = self.nodes[node].take() {
            volume.shutdown(&mut self.rings[node]).unwrap();
        }
    }
}

impl Drop for Cluster {
    fn drop(&mut self) {
        for node in 0..self.nodes.len() {
            self.remove(node);
        }
        self.stop.store(true, std::sync::atomic::Ordering::Relaxed);
        for backend in self.backends.drain(..) {
            backend.join().unwrap();
        }
    }
}

#[test]
fn topology_three_edges_intermediate_cache_and_original_target() {
    let Some(mut c) = Cluster::new() else { return };
    let config = c.config(0);
    let volume = &config.volumes()[0];
    let target = (0..)
        .map(|n| c.target(7, &format!("three-edges-{n}")))
        .find(|target| {
            let key = crate::cache::PeerDescriptor::page(
                target,
                crate::cache::PeerPage::new(
                    0,
                    3,
                    crate::metadata::Checksum(*blake3::hash(b"abc").as_bytes()),
                ),
            )
            .key(volume.namespace(&config.config_snapshot().universe))
            .unwrap();
            volume.routing().start_key(&key).owner == 7
        })
        .unwrap();
    // Canonical 0 -> 1 -> 3 -> 7: exactly two intermediate nodes.
    assert_eq!(c.get(0, &target), (200, b"abc".to_vec()));
    let hits = c.hits.lock().unwrap().clone();
    assert_eq!(hits.len(), 2);
    for (node, request) in hits {
        assert_eq!(node, 7);
        assert!(request.contains(&format!(" {target} HTTP/1.1\r\n")));
    }
    assert!(c.hits.lock().unwrap()[1].1.contains("Range: bytes=0-2\r\n"));
    c.remove(7);
    assert_eq!(c.get(1, &target), (200, b"abc".to_vec()));
    assert_eq!(c.get(3, &target), (200, b"abc".to_vec()));
    assert_eq!(
        c.hits.lock().unwrap().len(),
        2,
        "intermediates retain both metadata and payload"
    );
}

#[test]
fn multiple_pages_stripe_and_refill_on_independent_owner_failure() {
    let Some(mut c) = Cluster::new() else { return };
    let config = c.config(0);
    let volume = &config.volumes()[0];
    let namespace = volume.namespace(&config.config_snapshot().universe);
    let size = crate::buffers::BUFFER_SIZE as u64;
    let owners = |target: &str| {
        (0..3)
            .map(|n| {
                let key = crate::cache::PeerDescriptor::page(
                    target,
                    crate::cache::PeerPage::new(
                        n * size,
                        2 * size + 17,
                        crate::metadata::Checksum([7; 32]),
                    ),
                )
                .key(namespace)
                .unwrap();
                volume.routing().start_key(&key).owner
            })
            .collect::<Vec<_>>()
    };
    let target = (0..)
        .map(|n| c.target(7, &format!("multipage-{n}")))
        .find(|target| {
            let owners = owners(target);
            // Losing slot 4 leaves the routes to metadata slot 7 and
            // successor slot 5 intact. Do not depend on a lucky hash domain.
            owners[1] == 4
                && owners.iter().all(|&o| o > 0 && o < 6)
                && owners
                    .iter()
                    .collect::<std::collections::BTreeSet<_>>()
                    .len()
                    == 3
        })
        .unwrap();
    let owners = owners(&target);
    // A cold page's primary is down. Only that page advances to its successor.
    c.remove(owners[1] as usize);
    for n in 0..3 {
        let range = format!("bytes={}-{}", n * size, n * size);
        assert_eq!(
            c.get_headers(0, &target, &[("Range", &range)]),
            (206, vec![n as u8 + 1])
        );
    }
    let hits = c.hits.lock().unwrap().clone();
    assert_eq!(hits.len(), 4);
    assert_eq!(hits[0].0, 7);
    assert!(hits[0].1.starts_with("HEAD "));
    for n in 0..3 {
        let expected = owners[n] + u32::from(n == 1);
        assert_eq!(hits[n + 1].0, expected as usize, "page {n} owner");
        let finish = ((n as u64 + 1) * size).min(2 * size + 17) - 1;
        assert!(
            hits[n + 1]
                .1
                .contains(&format!("Range: bytes={}-{finish}\r\n", n as u64 * size))
        );
    }
    c.remove(7);
    for owner in owners {
        c.remove(owner as usize);
    }
    for n in 0..3 {
        let range = format!("bytes={}-{}", n * size, n * size);
        assert_eq!(
            c.get_headers(0, &target, &[("Range", &range)]),
            (206, vec![n as u8 + 1])
        );
    }
    assert_eq!(
        c.hits.lock().unwrap().len(),
        4,
        "all pages warm after owner loss"
    );
}

#[test]
fn metadata_and_page_use_distinct_physical_owners_and_warm_cache() {
    let Some(mut c) = Cluster::new() else { return };
    let config = c.config(0);
    let volume = &config.volumes()[0];
    let mut page_owner = 0;
    let target = (0..)
        .map(|n| c.target(7, &format!("striped-{n}")))
        .find(|target| {
            let key = crate::cache::PeerDescriptor::page(
                target,
                crate::cache::PeerPage::new(
                    0,
                    3,
                    crate::metadata::Checksum(*blake3::hash(b"abc").as_bytes()),
                ),
            )
            .key(volume.namespace(&config.config_snapshot().universe))
            .unwrap();
            page_owner = volume.routing().start_key(&key).owner;
            page_owner != 7 && page_owner != 0
        })
        .unwrap();
    assert_eq!(c.get(0, &target), (200, b"abc".to_vec()));
    let hits = c.hits.lock().unwrap().clone();
    assert_eq!(hits.len(), 2);
    assert_eq!(hits[0].0, 7, "metadata primary");
    assert!(hits[0].1.starts_with("HEAD "));
    assert_eq!(hits[1].0, page_owner as usize, "independent page primary");
    assert!(hits[1].1.contains("Range: bytes=0-2\r\n"));
    c.remove(7);
    c.remove(page_owner as usize);
    assert_eq!(c.get(0, &target), (200, b"abc".to_vec()));
    assert_eq!(
        c.hits.lock().unwrap().len(),
        2,
        "warm ingress survives both owners"
    );
}

use super::*;
use crate::control::{
    proto,
    tests::{cluster_config, fixture, prepare_snapshot, ring, runtime_pair as prepared},
};
pub(super) fn address() -> SocketAddr {
    static NEXT: std::sync::atomic::AtomicU32 = std::sync::atomic::AtomicU32::new(1);
    let n = NEXT.fetch_add(1, std::sync::atomic::Ordering::Relaxed);
    assert!(n < 65536);
    std::net::TcpListener::bind(SocketAddr::from(([127, 64, (n >> 8) as u8, n as u8], 0)))
        .unwrap()
        .local_addr()
        .unwrap()
}

pub(super) fn peer_address(mut address: SocketAddr) -> SocketAddr {
    address.set_port(9443);
    address
}

fn peer_endpoints(config: &mut proto::Snapshot) {
    for peer in &mut config.peers {
        peer.http_address = peer_address(peer.http_address.parse().unwrap()).to_string();
    }
    for volume in &mut config.volumes {
        if let Some(endpoints) = &mut volume.peer_endpoints {
            for endpoint in &mut endpoints.peers {
                if !endpoint.http_address.is_empty() {
                    endpoint.http_address =
                        peer_address(endpoint.http_address.parse().unwrap()).to_string();
                }
            }
        }
    }
}

pub(super) fn tls_provider(
    universe: &[u8],
    node: &[u8],
    pod_uid: &str,
) -> Arc<crate::control::credentials::Provider> {
    thread_local! {
        static AUTHORITY: crate::tls::tests::Authority = crate::tls::tests::Authority::new();
    }
    let identity = crate::tls::PeerIdentity::new(
        &crate::cache::peer_wire::hex(universe),
        &crate::cache::peer_wire::hex(node),
        pod_uid,
    )
    .unwrap();
    AUTHORITY.with(|authority| {
        let context = authority.context(&identity, true);
        crate::control::credentials::Provider::for_test(identity, Arc::new(context))
    })
}
pub(crate) fn activate(
    ring: &mut uring::Ring,
    config: Prepared,
    address: SocketAddr,
    worker: usize,
    rails: negotiation::Rails,
) -> Volumes {
    let updates = Arc::new(Updates::default());
    let provider = tls_provider(
        &config.config_snapshot().universe,
        &config.config_snapshot().node,
        "test-pod",
    );
    updates.set_credentials(provider);
    let mut snapshot = config.config_snapshot().clone();
    peer_endpoints(&mut snapshot);
    let trust = crate::control::Trust {
        universe: snapshot.universe.as_slice().try_into().unwrap(),
        node: snapshot.node.as_slice().try_into().unwrap(),
    };
    let config = prepare_snapshot(&trust, snapshot);
    for _ in 0..=worker {
        updates.subscribe(ring.wake_handle());
    }
    updates.publish(config).unwrap();
    for other in 0..worker {
        updates.staged(1, other, true);
    }
    let crypto = Arc::new(crate::crypto::Pool::test_pool(ring.pool()));

    let mut volumes = Volumes::new(crate::cache::tests::cache(1), updates, crypto, worker)
        .with_peer_ip(address.ip())
        .with_rdma(Some(rails));

    volumes.poll(ring, 32).unwrap();
    for other in 0..worker {
        volumes.updates.activated(1, other);
    }
    volumes
}

#[test]
fn sparse_failure_backoff_and_barrier_do_not_activate_staged_policy() {
    let Some(mut ring) = ring() else { return };
    let addr = address();
    let updates = Arc::new(Updates::default());
    let (trust, _) = fixture();
    updates.set_credentials(tls_provider(&trust.universe, &trust.node, "test-pod"));
    updates.subscribe(ring.wake_handle());
    updates.subscribe(ring.wake_handle());
    updates.publish(prepared(2, addr, address(), true)).unwrap();
    let crypto = Arc::new(crate::crypto::Pool::test_pool(ring.pool()));
    let mut volumes = Volumes::new(crate::cache::tests::cache(1), updates.clone(), crypto, 0)
        .with_peer_ip(addr.ip())
        .with_rdma(Some(negotiation::Rails::new(vec![None; 3], 3).unwrap()));
    volumes.poll(&mut ring, 16).unwrap();
    assert!(volumes.servers.is_empty());
    let staged = volumes.staged.as_ref().unwrap().generations[&local_key(addr)].clone();
    assert!(!staged.active.get());
    updates.staged(1, 1, true);
    volumes.poll(&mut ring, 16).unwrap();
    updates.activated(1, 1);
    assert!(staged.active.get());
    manager_mut(&staged).trigger(&staged._config.volumes()[0].config().peers[0], "/object");
    volumes.poll(&mut ring, 16).unwrap();
    let after = manager(&staged).outbound[0].retry.after;
    for _ in 0..5 {
        manager_mut(&staged).trigger(&staged._config.volumes()[0].config().peers[0], "/different");
        volumes.poll(&mut ring, 16).unwrap();
    }
    let manager = manager(&staged);
    assert_eq!(manager.outbound.len(), 1);
    assert_eq!(manager.outbound[0].retry.after, after);
    assert_eq!(manager.rails.total(), 3);
    assert!(manager.live.is_empty());
    drop(manager);
    let (trust, _) = fixture();
    let mut config = staged._config.config_snapshot().clone();
    config.revision = 2;
    config.fabric.clear();
    updates.publish(prepare_snapshot(&trust, config)).unwrap();
    volumes.poll(&mut ring, 16).unwrap();
    let disabled = volumes.staged.as_ref().unwrap().generations[&local_key(addr)].clone();
    assert!(disabled.manager.is_none());
    assert!(!disabled.active.get());
    assert!(staged.active.get());
    updates.staged(2, 1, true);
    volumes.poll(&mut ring, 16).unwrap();
    assert!(disabled.active.get());
    assert!(!staged.active.get());
    volumes.shutdown(&mut ring).unwrap();
}
#[test]
fn inbound_membership_shard_cardinality_and_expiry() {
    let Some(mut ring) = ring() else { return };
    let addr = address();
    let mut volumes = activate(
        &mut ring,
        prepared(3, addr, address(), false),
        addr,
        9,
        negotiation::Rails::new(vec![None], 1).unwrap(),
    );
    let generation = generation(&volumes, addr);
    let mut manager = manager_mut(&generation);
    let mut hint = negotiation::RequestHint {
        node: NodeId::from_bytes(&[2; 32]).unwrap(),
        volume: manager.context.volume(),
        shard: 1000,
        is_finish: false,
    };
    let first = manager.incoming(&hint).unwrap();
    assert!(Rc::ptr_eq(&first, &manager.incoming(&hint).unwrap()));
    hint.node = NodeId::from_bytes(&[8; 32]).unwrap();
    assert!(manager.incoming(&hint).is_err());
    hint.node = NodeId::from_bytes(&[2; 32]).unwrap();
    for shard in 0..MAX_PATHS - 1 {
        hint.shard = shard as u64;
        manager.incoming(&hint).unwrap();
    }
    hint.shard = u64::MAX;
    assert!(manager.incoming(&hint).is_err());
    manager.poll(&generation, &mut ring, 16);
    assert!(manager.inbound.is_empty()); // failed/empty reservations don't accumulate
    drop(manager);
    volumes.shutdown(&mut ring).unwrap();
}

#[test]
fn reloads_all_volumes_and_peers_and_failed_bind_preserves_generation() {
    let Some(mut ring) = ring() else { return };
    let (trust, mut config) = fixture();
    let first = address();
    config.volumes[0].cache_socket = crate::control::tests::test_socket(first, "cache");
    let updates = Arc::new(Updates::default());
    updates.subscribe(ring.wake_handle());
    let crypto = Arc::new(crate::crypto::Pool::test_pool(ring.pool()));
    let mut cache = crate::cache::tests::cache(1);
    cache.set_metrics(ring.metrics().clone());
    let mut volumes = Volumes::new(cache, updates.clone(), crypto.clone(), 0);
    let prepare = |s| prepare_snapshot(&trust, s);
    updates.publish(prepare(config.clone())).unwrap();
    volumes.poll(&mut ring, 16).unwrap();
    let old = generation(&volumes, first);
    assert_eq!(old._config.config_snapshot().revision, 1);
    old.handlers[0]
        .borrow_mut()
        .cache_mut()
        .metrics()
        .request(crate::metrics::Traffic::ClientHttp);
    config.revision = 2;
    config.volumes[0].peers.clear();
    config.volumes[0].topology = Some(proto::Topology {
        routing_algorithm: Some(1),
        epoch: 2,
        slot_count: 2,
        local_slots: vec![0, 1],
        neighbors: vec![],
    });
    config.peers.clear();
    let second = address();
    let mut extra = config.volumes[0].clone();
    extra.id = "second".into();
    extra.cache_socket = crate::control::tests::test_socket(second, "cache");
    extra.origin_socket = crate::control::tests::test_socket(second, "origin");
    config.volumes.push(extra);
    updates.publish(prepare(config.clone())).unwrap();
    volumes.poll(&mut ring, 16).unwrap();
    assert_eq!(volumes.servers.len(), 2);
    assert!(generation(&volumes, first)._config.peers().is_empty());
    assert_eq!(old._config.peers().len(), 1);
    for server in volumes.servers.values() {
        for handler in &server.handler().current.handlers {
            handler
                .borrow_mut()
                .cache_mut()
                .metrics()
                .request(crate::metrics::Traffic::ClientHttp);
        }
    }
    assert_eq!(
        ring.metrics().values()[0],
        3,
        "old and both new volume handlers retain worker counters"
    );
    let occupied_path = crate::control::tests::test_socket(address(), "cache");
    let occupied = std::os::unix::net::UnixListener::bind(&occupied_path).unwrap();
    config.revision = 3;
    config.volumes[1].cache_socket = occupied_path.clone();
    updates.publish(prepare(config.clone())).unwrap();
    volumes.poll(&mut ring, 16).unwrap();
    assert_eq!(updates.decision(3), Decision::Waiting);
    assert_eq!(
        generation(&volumes, first)
            ._config
            .config_snapshot()
            .revision,
        2
    );
    assert!(volumes.servers.contains_key(&local_key(second)));
    config.revision = 4;
    config.volumes.clear();
    updates.publish(prepare(config)).unwrap();
    volumes.poll(&mut ring, 16).unwrap();
    assert!(volumes.servers.is_empty());
    drop(old);
    volumes.shutdown(&mut ring).unwrap();
    drop(volumes);
    Arc::try_unwrap(crypto).ok().unwrap().shutdown().unwrap();
    drop(occupied);
    std::fs::remove_file(occupied_path).unwrap();
}
