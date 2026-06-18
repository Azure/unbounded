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
    pub neighborhood_id: Option<String>,
    pub bypass_cache: bool,
}

#[derive(Debug, Clone, PartialEq)]
pub struct RuntimeP2p {
    pub fingers_per_node: u32,
    pub local_node_id: Option<u64>,
    pub local_tags: Vec<String>,
    pub routing_plan: Option<RoutingPlan>,
}

#[derive(Debug, Clone, PartialEq)]
pub struct RuntimeCache {
    pub id: String,
    pub disks: Vec<DiskSpec>,
}

#[derive(Debug, Clone, PartialEq)]
pub struct RuntimePeer {
    pub neighborhood_id: String,
    pub node_id: NodeId,
    pub fabric_peer_id: PeerId,
    pub spec: PeerSpec,
}

#[derive(Debug, Clone, PartialEq)]
pub struct RuntimeNeighborhood {
    pub id: String,
    pub backend_id: String,
    pub p2p: RuntimeP2p,
    pub peers: Vec<RuntimePeer>,
}

#[derive(Debug, Clone, PartialEq)]
pub struct RuntimeGraph {
    pub caches: HashMap<String, RuntimeCache>,
    pub neighborhoods: HashMap<String, RuntimeNeighborhood>,
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
    for n in &config.neighborhoods {
        insert_id(&mut ids, "neighborhood", &n.name)?;
        require_source("neighborhood", &n.name, &n.source)?;
    }
    for f in &config.frontends {
        insert_id(&mut ids, "frontend", &f.name)?;
        require_source("frontend", &f.name, &f.source)?;
    }

    let backends = by_id(&config.backends, |b| b.name.as_str());
    let caches = by_id(&config.caches, |c| c.name.as_str());
    let neighborhoods = by_id(&config.neighborhoods, |n| n.name.as_str());

    for c in &config.caches {
        if !neighborhoods.contains_key(c.source.as_str())
            && !backends.contains_key(c.source.as_str())
        {
            return Err(format!(
                "cache {:?} source {:?}, which is not a backend or neighborhood",
                c.name, c.source
            ));
        }
    }
    for n in &config.neighborhoods {
        if !backends.contains_key(n.source.as_str()) {
            return Err(format!(
                "neighborhood {:?} source {:?}, which is not a backend",
                n.name, n.source
            ));
        }
    }
    for f in &config.frontends {
        if !backends.contains_key(f.source.as_str())
            && !caches.contains_key(f.source.as_str())
            && !neighborhoods.contains_key(f.source.as_str())
        {
            return Err(format!(
                "frontend {:?} source {:?}, which is not a backend, cache, or neighborhood",
                f.name, f.source
            ));
        }
    }

    Ok(())
}

pub fn runtime_projection(config: &Config) -> Result<RuntimeGraph, String> {
    validate_binding_graph(config)?;

    let backends = by_id(&config.backends, |b| b.name.as_str());
    let caches = by_id(&config.caches, |c| c.name.as_str());
    let neighborhoods = by_id(&config.neighborhoods, |n| n.name.as_str());

    let mut bindings = HashMap::new();

    for frontend in &config.frontends {
        let binding = if backends.contains_key(frontend.source.as_str()) {
            ResolvedFrontendBinding {
                frontend_id: frontend.name.clone(),
                backend_id: frontend.source.clone(),
                cache_id: None,
                neighborhood_id: None,
                bypass_cache: true,
            }
        } else if let Some(cache) = caches.get(frontend.source.as_str()) {
            let (backend_id, neighborhood_id) =
                if let Some(neighborhood) = neighborhoods.get(cache.source.as_str()) {
                    (neighborhood.source.clone(), Some(neighborhood.name.clone()))
                } else {
                    (cache.source.clone(), None)
                };
            ResolvedFrontendBinding {
                frontend_id: frontend.name.clone(),
                backend_id,
                cache_id: Some(cache.name.clone()),
                neighborhood_id,
                bypass_cache: false,
            }
        } else {
            let neighborhood = neighborhoods
                .get(frontend.source.as_str())
                .expect("binding graph validation checked frontend target");
            ResolvedFrontendBinding {
                frontend_id: frontend.name.clone(),
                backend_id: neighborhood.source.clone(),
                cache_id: None,
                neighborhood_id: Some(neighborhood.name.clone()),
                bypass_cache: false,
            }
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
                    disks: cache.disks.clone(),
                },
            )
        })
        .collect();

    let neighborhoods = config
        .neighborhoods
        .iter()
        .map(|neighborhood| {
            let peers = neighborhood
                .peers
                .iter()
                .map(|peer| RuntimePeer {
                    neighborhood_id: neighborhood.name.clone(),
                    node_id: NodeId(peer.id),
                    fabric_peer_id: scoped_peer_id(&neighborhood.name, peer.id),
                    spec: peer.clone(),
                })
                .collect();
            (
                neighborhood.name.clone(),
                RuntimeNeighborhood {
                    id: neighborhood.name.clone(),
                    backend_id: neighborhood.source.clone(),
                    p2p: RuntimeP2p {
                        fingers_per_node: neighborhood.fingers_per_node.max(1),
                        local_node_id: neighborhood.local_node_id,
                        local_tags: neighborhood.local_tags.clone(),
                        routing_plan: neighborhood.routing_plan.clone(),
                    },
                    peers,
                },
            )
        })
        .collect();

    Ok(RuntimeGraph {
        caches,
        neighborhoods,
        frontends: bindings,
    })
}

pub fn empty_runtime_p2p() -> RuntimeP2p {
    RuntimeP2p {
        fingers_per_node: 100,
        local_node_id: None,
        local_tags: Vec::new(),
        routing_plan: None,
    }
}

pub fn scoped_peer_id(neighborhood_id: &str, node_id: u64) -> PeerId {
    let mut hasher = blake3::Hasher::new();
    hasher.update(b"unbounded-storage scoped peer id v1");
    hasher.update(&(neighborhood_id.len() as u64).to_le_bytes());
    hasher.update(neighborhood_id.as_bytes());
    hasher.update(&node_id.to_le_bytes());
    let digest = hasher.finalize();
    let mut bytes = [0u8; 8];
    bytes.copy_from_slice(&digest.as_bytes()[0..8]);
    let id = u64::from_le_bytes(bytes);
    if id == 0 { PeerId(1) } else { PeerId(id) }
}

#[allow(dead_code)]
pub fn legacy_empty_projection() -> RuntimeGraph {
    RuntimeGraph {
        caches: HashMap::new(),
        neighborhoods: HashMap::new(),
        frontends: HashMap::new(),
    }
}

pub fn runtime_disks(graph: &RuntimeGraph) -> Vec<DiskSpec> {
    let mut disks = Vec::new();
    for cache in graph.caches.values() {
        disks.extend(cache.disks.clone());
    }
    disks
}

pub fn runtime_peers(graph: &RuntimeGraph) -> Vec<RuntimePeer> {
    let mut peers = Vec::new();
    for neighborhood in graph.neighborhoods.values() {
        peers.extend(neighborhood.peers.clone());
    }
    peers
}

pub fn runtime_p2p_or_default(graph: &RuntimeGraph) -> RuntimeP2p {
    match graph.neighborhoods.values().next() {
        Some(neighborhood) => neighborhood.p2p.clone(),
        None => empty_runtime_p2p(),
    }
}

pub fn first_runtime_neighborhood(graph: &RuntimeGraph) -> Option<&RuntimeNeighborhood> {
    graph.neighborhoods.values().next()
}

pub fn first_runtime_cache(graph: &RuntimeGraph) -> Option<&RuntimeCache> {
    graph.caches.values().next()
}

pub fn compat_runtime_projection(config: &Config) -> Result<CompatRuntimeProjection, String> {
    let graph = runtime_projection(config)?;
    let (p2p, peers) = match first_runtime_neighborhood(&graph) {
        Some(neighborhood) => (
            neighborhood.p2p.clone(),
            neighborhood.peers.iter().map(|p| p.spec.clone()).collect(),
        ),
        None => (
            RuntimeP2p {
                fingers_per_node: 100,
                local_node_id: None,
                local_tags: Vec::new(),
                routing_plan: None,
            },
            Vec::new(),
        ),
    };
    Ok(CompatRuntimeProjection {
        p2p,
        peers,
        disks: runtime_disks(&graph),
        frontends: graph.frontends,
    })
}

#[derive(Debug, Clone, PartialEq)]
pub struct CompatRuntimeProjection {
    pub p2p: RuntimeP2p,
    pub peers: Vec<PeerSpec>,
    pub disks: Vec<DiskSpec>,
    pub frontends: HashMap<String, ResolvedFrontendBinding>,
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
        BackendSpec, CacheSpec, FrontendSpec, HttpBackendConfig, HttpFrontendConfig,
        NeighborhoodSpec, backend_spec, frontend_spec,
    };
    use super::*;

    fn backend(id: &str) -> BackendSpec {
        BackendSpec {
            name: id.to_string(),
            config: Some(backend_spec::Config::Http(HttpBackendConfig {
                url: "https://example.com".to_string(),
                stripe_size_bytes: 4 * 1024 * 1024,
                http_concurrency: 64,
            })),
        }
    }

    fn cache(id: &str, source: &str) -> CacheSpec {
        CacheSpec {
            name: id.to_string(),
            source: source.to_string(),
            disks: Vec::new(),
        }
    }

    fn neighborhood(id: &str, source: &str) -> NeighborhoodSpec {
        NeighborhoodSpec {
            name: id.to_string(),
            source: source.to_string(),
            fingers_per_node: 100,
            local_node_id: Some(1),
            local_tags: Vec::new(),
            routing_plan: None,
            peers: Vec::new(),
        }
    }

    fn frontend(id: &str, source: &str) -> FrontendSpec {
        FrontendSpec {
            name: id.to_string(),
            source: source.to_string(),
            config: Some(frontend_spec::Config::Http(HttpFrontendConfig {
                addr: format!("127.0.0.1:{}", 9000 + id.len()),
            })),
        }
    }

    #[test]
    fn runtime_projection_accepts_all_chain_shapes() {
        let mut cfg = Config::default();
        cfg.backends.push(backend("b"));
        cfg.caches.push(cache("c-backend", "b"));
        cfg.neighborhoods.push(neighborhood("n", "b"));
        cfg.caches.push(cache("c-neighborhood", "n"));
        cfg.frontends.push(frontend("direct", "b"));
        cfg.frontends.push(frontend("cache", "c-backend"));
        cfg.frontends.push(frontend("neighborhood", "n"));
        cfg.frontends.push(frontend("full", "c-neighborhood"));

        let graph = runtime_projection(&cfg).unwrap();

        assert_binding(&graph, "direct", "b", None, None, true);
        assert_binding(&graph, "cache", "b", Some("c-backend"), None, false);
        assert_binding(&graph, "neighborhood", "b", None, Some("n"), false);
        assert_binding(
            &graph,
            "full",
            "b",
            Some("c-neighborhood"),
            Some("n"),
            false,
        );
        assert_eq!(graph.caches.len(), 2);
        assert_eq!(graph.neighborhoods.len(), 1);
    }

    #[test]
    fn scoped_peer_ids_are_per_neighborhood() {
        let a = scoped_peer_id("n-a", 7);
        let b = scoped_peer_id("n-b", 7);
        let c = scoped_peer_id("n-a", 7);

        assert_ne!(a, b);
        assert_eq!(a, c);
        assert_ne!(a, PeerId(0));
    }

    fn assert_binding(
        graph: &RuntimeGraph,
        frontend_id: &str,
        backend_id: &str,
        cache_id: Option<&str>,
        neighborhood_id: Option<&str>,
        bypass_cache: bool,
    ) {
        let binding = graph.frontends.get(frontend_id).unwrap();
        assert_eq!(binding.backend_id, backend_id);
        assert_eq!(binding.cache_id.as_deref(), cache_id);
        assert_eq!(binding.neighborhood_id.as_deref(), neighborhood_id);
        assert_eq!(binding.bypass_cache, bypass_cache);
    }
}
