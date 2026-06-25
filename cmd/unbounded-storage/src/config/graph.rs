// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Binding graph validation and runtime projection.

use std::collections::{HashMap, HashSet};

use super::schema::{Config, DiskSpec, PeerSpec, RoutingPlan, TopologyWeighting};
use crate::fabric::PeerId;
use crate::p2p::{NodeId, node_id_from_name};

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ResolvedFrontendBinding {
    pub frontend_id: String,
    pub backend_id: String,
    pub cache_id: Option<String>,
    pub bypass_cache: bool,
}

#[derive(Debug, Clone, PartialEq)]
pub struct RuntimeMesh {
    pub fingers_per_node: u32,
    pub self_name: Option<String>,
    pub self_node_id: NodeId,
    pub self_peer_id: PeerId,
    pub self_tags: Vec<String>,
    pub routing_plan: Option<RoutingPlan>,
    pub topology_weighting: Option<TopologyWeighting>,
    pub peers: Vec<RuntimePeer>,
}

#[derive(Debug, Clone, PartialEq)]
pub struct RuntimeCache {
    pub id: String,
    pub backend_id: String,
}

#[derive(Debug, Clone, PartialEq)]
pub struct RuntimePeer {
    pub name: String,
    pub node_id: NodeId,
    pub fabric_peer_id: PeerId,
    pub spec: PeerSpec,
}

#[derive(Debug, Clone, PartialEq)]
pub struct RuntimeGraph {
    pub disks: Vec<DiskSpec>,
    pub caches: HashMap<String, RuntimeCache>,
    pub mesh: RuntimeMesh,
    pub frontends: HashMap<String, ResolvedFrontendBinding>,
}

pub fn validate_binding_graph(config: &Config) -> Result<(), String> {
    let mut ids = HashSet::new();
    for b in &config.backends {
        insert_id(&mut ids, "backend", &b.name)?;
    }
    for c in &config.caches {
        insert_id(&mut ids, "cache", &c.name)?;
        require_source("cache", &c.name, &c.source)?;
    }
    for f in &config.frontends {
        insert_id(&mut ids, "frontend", &f.name)?;
        require_source("frontend", &f.name, &f.source)?;
    }

    let backends = by_id(&config.backends, |b| b.name.as_str());
    let caches = by_id(&config.caches, |c| c.name.as_str());

    for c in &config.caches {
        if !backends.contains_key(c.source.as_str()) {
            return Err(format!(
                "cache {:?} source {:?}, which is not a backend",
                c.name, c.source
            ));
        }
    }
    validate_peer_names(config)?;
    for f in &config.frontends {
        if !backends.contains_key(f.source.as_str()) && !caches.contains_key(f.source.as_str()) {
            return Err(format!(
                "frontend {:?} source {:?}, which is not a backend or cache",
                f.name, f.source
            ));
        }
    }

    Ok(())
}

fn validate_peer_names(config: &Config) -> Result<(), String> {
    let mut names = HashSet::new();
    let mut derived_ids = HashMap::new();
    let mut self_seen = config.self_.is_empty();

    for peer in &config.peers {
        if peer.name.is_empty() {
            return Err("peer name must not be empty".to_string());
        }
        if !names.insert(peer.name.as_str()) {
            return Err(format!("peer {:?} is duplicated", peer.name));
        }
        if peer.name == config.self_ {
            self_seen = true;
        }
        let node_id = node_id_from_name(&peer.name);
        if let Some(prev) = derived_ids.insert(node_id, peer.name.as_str()) {
            return Err(format!(
                "peer names {prev:?} and {:?} derive the same node id {}",
                peer.name, node_id.0
            ));
        }
    }
    if !self_seen {
        return Err(format!(
            "self peer {:?} is not present in peers",
            config.self_
        ));
    }

    Ok(())
}

pub fn runtime_projection(config: &Config) -> Result<RuntimeGraph, String> {
    validate_binding_graph(config)?;

    let backends = by_id(&config.backends, |b| b.name.as_str());
    let caches = by_id(&config.caches, |c| c.name.as_str());

    let mut bindings = HashMap::new();

    for frontend in &config.frontends {
        let binding = if backends.contains_key(frontend.source.as_str()) {
            ResolvedFrontendBinding {
                frontend_id: frontend.name.clone(),
                backend_id: frontend.source.clone(),
                cache_id: None,
                bypass_cache: true,
            }
        } else if let Some(cache) = caches.get(frontend.source.as_str()) {
            ResolvedFrontendBinding {
                frontend_id: frontend.name.clone(),
                backend_id: cache.source.clone(),
                cache_id: Some(cache.name.clone()),
                bypass_cache: false,
            }
        } else {
            unreachable!("binding graph validation checked frontend target")
        };
        bindings.insert(frontend.name.clone(), binding);
    }

    let caches = config
        .caches
        .iter()
        .map(|cache| {
            (
                cache.name.clone(),
                RuntimeCache {
                    id: cache.name.clone(),
                    backend_id: cache.source.clone(),
                },
            )
        })
        .collect();

    let self_peer = config
        .peers
        .iter()
        .find(|peer| !config.self_.is_empty() && peer.name == config.self_);
    let self_node_id = self_peer
        .map(|peer| node_id_from_name(&peer.name))
        .unwrap_or(NodeId(0));
    let self_peer_id = PeerId(self_node_id.0);
    let self_tags = self_peer.map(|peer| peer.tags.clone()).unwrap_or_default();
    let self_name = (!config.self_.is_empty()).then(|| config.self_.clone());

    let peers = config
        .peers
        .iter()
        .filter(|peer| Some(peer.name.as_str()) != self_name.as_deref())
        .map(|peer| {
            let node_id = node_id_from_name(&peer.name);
            RuntimePeer {
                name: peer.name.clone(),
                node_id,
                fabric_peer_id: PeerId(node_id.0),
                spec: peer.clone(),
            }
        })
        .collect();

    Ok(RuntimeGraph {
        disks: config.disks.clone(),
        caches,
        mesh: RuntimeMesh {
            fingers_per_node: config.fingers_per_node.unwrap_or(100).max(1),
            self_name,
            self_node_id,
            self_peer_id,
            self_tags,
            routing_plan: config.routing_plan.clone(),
            topology_weighting: config.topology_weighting.clone(),
            peers,
        },
        frontends: bindings,
    })
}

pub fn runtime_disks(graph: &RuntimeGraph) -> Vec<DiskSpec> {
    graph.disks.clone()
}

pub fn runtime_peers(graph: &RuntimeGraph) -> Vec<RuntimePeer> {
    graph.mesh.peers.clone()
}

pub fn frontend_backend_map(
    bindings: &HashMap<String, ResolvedFrontendBinding>,
) -> HashMap<String, String> {
    bindings
        .iter()
        .map(|(id, binding)| (id.clone(), binding.backend_id.clone()))
        .collect()
}

fn insert_id(ids: &mut HashSet<String>, kind: &str, name: &str) -> Result<(), String> {
    if name.is_empty() {
        return Ok(());
    }
    if !ids.insert(name.to_string()) {
        return Err(format!(
            "duplicate component name {name:?} while adding {kind}"
        ));
    }
    Ok(())
}

fn require_source(kind: &str, name: &str, source: &str) -> Result<(), String> {
    if source.is_empty() {
        return Err(format!("{kind} {name:?}: source must not be empty"));
    }
    Ok(())
}

fn by_id<'a, T>(items: &'a [T], id: impl Fn(&'a T) -> &'a str) -> HashMap<&'a str, &'a T> {
    items.iter().map(|item| (id(item), item)).collect()
}

#[cfg(test)]
mod tests {
    use super::super::schema::{
        BackendSpec, CacheSpec, FrontendSpec, HttpBackendConfig, HttpFrontendConfig, PeerSpec,
        RdmaPeerConfig, TcpPeerConfig, TopologyPrefixWeight, TopologyWeighting, backend_spec,
        frontend_spec, peer_spec,
    };
    use super::*;

    fn backend(id: &str) -> BackendSpec {
        BackendSpec {
            name: id.to_string(),
            config: Some(backend_spec::Config::Http(HttpBackendConfig {
                url: "https://example.com".to_string(),
                stripe_size_bytes: Some(4 * 1024 * 1024),
                http_concurrency: Some(64),
                metadata_ttl_default_secs: Some(60),
                metadata_ttl_max_secs: Some(60),
                not_found_ttl_default_secs: Some(5),
                not_found_ttl_max_secs: Some(5),
                ca_cert_path: None,
                insecure_skip_verify: false,
                client_cert_path: None,
                client_key_path: None,
            })),
        }
    }

    fn cache(id: &str, source: &str) -> CacheSpec {
        CacheSpec {
            name: id.to_string(),
            source: source.to_string(),
        }
    }

    fn frontend(id: &str, source: &str) -> FrontendSpec {
        FrontendSpec {
            name: id.to_string(),
            source: source.to_string(),
            config: Some(frontend_spec::Config::Http(HttpFrontendConfig {
                addr: format!("127.0.0.1:{}", 9000 + id.len()),
                max_requests_per_connection: None,
            })),
        }
    }

    fn tcp_peer(name: &str, addr: &str) -> PeerSpec {
        PeerSpec {
            name: name.to_string(),
            tags: Vec::new(),
            config: Some(peer_spec::Config::Tcp(TcpPeerConfig {
                addr: addr.to_string(),
            })),
        }
    }

    fn rdma_peer(name: &str, addr: &str) -> PeerSpec {
        PeerSpec {
            name: name.to_string(),
            tags: Vec::new(),
            config: Some(peer_spec::Config::Rdma(RdmaPeerConfig {
                addr: addr.to_string(),
                addrs: Vec::new(),
            })),
        }
    }

    #[test]
    fn runtime_projection_accepts_all_chain_shapes() {
        let mut cfg = Config::default();
        cfg.backends.push(backend("b"));
        cfg.caches.push(cache("c-backend", "b"));
        cfg.frontends.push(frontend("direct", "b"));
        cfg.frontends.push(frontend("cache", "c-backend"));

        let graph = runtime_projection(&cfg).unwrap();

        assert_binding(&graph, "direct", "b", None, true);
        assert_binding(&graph, "cache", "b", Some("c-backend"), false);
        assert_eq!(graph.caches.len(), 1);
    }

    #[test]
    fn runtime_projection_accepts_unique_rdma_peer_names() {
        let mut cfg = Config::default();
        cfg.backends.push(backend("b"));
        cfg.self_ = "node-a".to_string();
        cfg.peers.push(rdma_peer("node-a", "hex:00"));
        cfg.peers.push(rdma_peer("node-b", "hex:01"));

        let graph = runtime_projection(&cfg).unwrap();
        let peers = &graph.mesh.peers;

        assert_eq!(graph.mesh.self_name.as_deref(), Some("node-a"));
        assert_eq!(graph.mesh.self_node_id, node_id_from_name("node-a"));
        assert_eq!(
            graph.mesh.self_peer_id,
            PeerId(node_id_from_name("node-a").0)
        );
        assert_eq!(peers.len(), 1);
        assert_eq!(peers[0].name, "node-b");
        assert_eq!(peers[0].node_id, node_id_from_name("node-b"));
        assert_eq!(
            peers[0].fabric_peer_id,
            PeerId(node_id_from_name("node-b").0)
        );
    }

    #[test]
    fn runtime_projection_carries_topology_weighting() {
        let mut cfg = Config::default();
        cfg.backends.push(backend("b"));
        cfg.self_ = "node-a".to_string();
        cfg.peers.push(tcp_peer("node-a", "127.0.0.1:1"));
        cfg.topology_weighting = Some(TopologyWeighting {
            prefix_weights: vec![TopologyPrefixWeight {
                tag_index: 1,
                weight: -0.25,
            }],
        });

        let graph = runtime_projection(&cfg).unwrap();

        let weighting = graph
            .mesh
            .topology_weighting
            .as_ref()
            .expect("topology weighting projected");
        assert_eq!(weighting.prefix_weights[0].tag_index, 1);
        assert_eq!(weighting.prefix_weights[0].weight, -0.25);
    }

    #[test]
    fn runtime_projection_rejects_missing_self_peer() {
        let mut cfg = Config::default();
        cfg.backends.push(backend("b"));
        cfg.self_ = "node-a".to_string();
        cfg.peers.push(tcp_peer("node-b", "127.0.0.1:7"));

        let err = runtime_projection(&cfg).unwrap_err();

        assert!(err.contains("self peer"), "{err}");
    }

    #[test]
    fn runtime_projection_rejects_duplicate_peer_names() {
        let mut cfg = Config::default();
        cfg.backends.push(backend("b"));
        cfg.peers.push(tcp_peer("node-a", "127.0.0.1:1"));
        cfg.peers.push(tcp_peer("node-a", "127.0.0.1:2"));

        let err = runtime_projection(&cfg).unwrap_err();
        assert!(err.contains("peer \"node-a\" is duplicated"), "{err}");
    }

    #[test]
    fn runtime_projection_rejects_duplicate_rdma_peer_names() {
        let mut cfg = Config::default();
        cfg.backends.push(backend("b"));
        cfg.peers.push(rdma_peer("node-a", "hex:00"));
        cfg.peers.push(rdma_peer("node-a", "hex:01"));

        let err = runtime_projection(&cfg).unwrap_err();
        assert!(err.contains("peer \"node-a\" is duplicated"), "{err}");
    }

    fn assert_binding(
        graph: &RuntimeGraph,
        frontend_id: &str,
        backend_id: &str,
        cache_id: Option<&str>,
        bypass_cache: bool,
    ) {
        let binding = graph.frontends.get(frontend_id).unwrap();
        assert_eq!(binding.backend_id, backend_id);
        assert_eq!(binding.cache_id.as_deref(), cache_id);
        assert_eq!(binding.bypass_cache, bypass_cache);
    }
}
