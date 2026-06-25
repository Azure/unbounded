// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Binding graph validation and runtime projection.

use std::collections::{HashMap, HashSet};

use super::schema::{Config, DiskSpec, PeerSpec, RoutingPlan};
use crate::fabric::PeerId;
use crate::p2p::NodeId;

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
    pub local_node_id: Option<u64>,
    pub local_tags: Vec<String>,
    pub routing_plan: Option<RoutingPlan>,
    pub peers: Vec<RuntimePeer>,
}

#[derive(Debug, Clone, PartialEq)]
pub struct RuntimeCache {
    pub id: String,
    pub backend_id: String,
}

#[derive(Debug, Clone, PartialEq)]
pub struct RuntimePeer {
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
    validate_peer_ids(config)?;
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

fn validate_peer_ids(config: &Config) -> Result<(), String> {
    let mut seen = HashSet::new();

    for peer in &config.peers {
        if !seen.insert(peer.id) {
            return Err(format!("peer {} is duplicated", peer.id));
        }
        if Some(peer.id) == config.local_node_id {
            return Err(format!(
                "peer {} collides with process local_node_id {}",
                peer.id, peer.id
            ));
        }
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

    let peers = config
        .peers
        .iter()
        .map(|peer| RuntimePeer {
            node_id: NodeId(peer.id),
            fabric_peer_id: PeerId(peer.id),
            spec: peer.clone(),
        })
        .collect();

    Ok(RuntimeGraph {
        disks: config.disks.clone(),
        caches,
        mesh: RuntimeMesh {
            fingers_per_node: config.fingers_per_node.unwrap_or(100).max(1),
            local_node_id: config.local_node_id,
            local_tags: config.local_tags.clone(),
            routing_plan: config.routing_plan.clone(),
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
        RdmaPeerConfig, TcpPeerConfig, backend_spec, frontend_spec, peer_spec,
    };
    use super::*;

    fn backend(id: &str) -> BackendSpec {
        BackendSpec {
            name: id.to_string(),
            config: Some(backend_spec::Config::Http(HttpBackendConfig {
                url: "https://example.com".to_string(),
                stripe_size_bytes: Some(4 * 1024 * 1024),
                http_concurrency: Some(64),
                ca_cert_path: None,
                insecure_skip_verify: false,
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

    fn tcp_peer(id: u64, addr: &str) -> PeerSpec {
        PeerSpec {
            id,
            tags: Vec::new(),
            config: Some(peer_spec::Config::Tcp(TcpPeerConfig {
                addr: addr.to_string(),
            })),
        }
    }

    fn rdma_peer(id: u64, addr: &str) -> PeerSpec {
        PeerSpec {
            id,
            tags: Vec::new(),
            config: Some(peer_spec::Config::Rdma(RdmaPeerConfig {
                addr: addr.to_string(),
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
    fn runtime_projection_accepts_unique_rdma_peer_ids() {
        let mut cfg = Config::default();
        cfg.backends.push(backend("b"));
        cfg.peers.push(rdma_peer(7, "hex:00"));
        cfg.peers.push(rdma_peer(8, "hex:01"));

        let graph = runtime_projection(&cfg).unwrap();
        let peers = &graph.mesh.peers;

        assert_eq!(peers.len(), 2);
        assert_eq!(peers[0].node_id, NodeId(7));
        assert_eq!(peers[1].node_id, NodeId(8));
        assert_eq!(peers[0].fabric_peer_id, PeerId(7));
        assert_eq!(peers[1].fabric_peer_id, PeerId(8));
    }

    #[test]
    fn runtime_projection_rejects_peer_colliding_with_process_local_node_id() {
        let mut cfg = Config::default();
        cfg.backends.push(backend("b"));
        cfg.local_node_id = Some(7);
        cfg.peers.push(tcp_peer(7, "127.0.0.1:7"));

        let err = runtime_projection(&cfg).unwrap_err();

        assert!(err.contains("collides with process local_node_id"), "{err}");
    }

    #[test]
    fn runtime_projection_rejects_duplicate_peer_ids() {
        let mut cfg = Config::default();
        cfg.backends.push(backend("b"));
        cfg.peers.push(tcp_peer(7, "127.0.0.1:1"));
        cfg.peers.push(tcp_peer(7, "127.0.0.1:2"));

        let err = runtime_projection(&cfg).unwrap_err();
        assert!(err.contains("peer 7 is duplicated"), "{err}");
    }

    #[test]
    fn runtime_projection_rejects_duplicate_rdma_peer_ids() {
        let mut cfg = Config::default();
        cfg.backends.push(backend("b"));
        cfg.peers.push(rdma_peer(7, "hex:00"));
        cfg.peers.push(rdma_peer(7, "hex:01"));

        let err = runtime_projection(&cfg).unwrap_err();
        assert!(err.contains("peer 7 is duplicated"), "{err}");
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
