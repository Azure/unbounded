// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Binding graph validation and runtime projection.

use std::collections::{HashMap, HashSet};

use super::schema::{
    Config, DiskSpec, FrontendMount, KeyspaceRoute, PeerSpec, RoutingPlan,
};
use crate::fabric::PeerId;
use crate::p2p::NodeId;

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ResolvedKeyspaceRoute {
    pub key_prefix: String,
    pub backend_id: String,
    pub origin_prefix: String,
    pub stripe_size: u64,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ResolvedFrontendMount {
    pub public_prefix: String,
    pub keyspace_id: String,
    pub key_prefix: String,
    pub cache_id: Option<String>,
    pub neighborhood_id: Option<String>,
    pub bypass_cache: bool,
    pub routes: Vec<ResolvedKeyspaceRoute>,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ResolvedFrontendBinding {
    pub frontend_id: String,
    pub mounts: Vec<ResolvedFrontendMount>,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ResolvedObject {
    pub keyspace_id: String,
    pub key_object_id: String,
    pub backend_id: String,
    pub origin_object_id: String,
    pub cache_id: Option<String>,
    pub neighborhood_id: Option<String>,
    pub stripe_size: u64,
    pub bypass_cache: bool,
}

impl ResolvedFrontendBinding {
    pub fn resolve_path(&self, path: &str) -> Option<ResolvedObject> {
        let mount = self
            .mounts
            .iter()
            .find(|mount| prefix_suffix(path, &mount.public_prefix).is_some())?;
        let public_suffix = prefix_suffix(path, &mount.public_prefix)?;
        let key_object_id = join_prefix_suffix(&mount.key_prefix, public_suffix);
        let route = mount
            .routes
            .iter()
            .find(|route| prefix_suffix(&key_object_id, &route.key_prefix).is_some())?;
        let key_suffix = prefix_suffix(&key_object_id, &route.key_prefix)?.to_string();
        Some(ResolvedObject {
            keyspace_id: mount.keyspace_id.clone(),
            key_object_id,
            backend_id: route.backend_id.clone(),
            origin_object_id: join_prefix_suffix(&route.origin_prefix, &key_suffix),
            cache_id: mount.cache_id.clone(),
            neighborhood_id: mount.neighborhood_id.clone(),
            stripe_size: route.stripe_size,
            bypass_cache: mount.bypass_cache,
        })
    }
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
    pub keyspace_id: String,
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
    for k in &config.keyspaces {
        insert_id(&mut ids, "keyspace", &k.name)?;
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
        validate_frontend_mounts(&f.name, &f.mounts)?;
    }

    let backends = by_id(&config.backends, |b| b.name.as_str());
    let keyspaces = by_id(&config.keyspaces, |k| k.name.as_str());
    let caches = by_id(&config.caches, |c| c.name.as_str());
    let neighborhoods = by_id(&config.neighborhoods, |n| n.name.as_str());

    for keyspace in &config.keyspaces {
        if keyspace.routes.is_empty() {
            return Err(format!(
                "keyspace {:?}: at least one route is required",
                keyspace.name
            ));
        }
        validate_keyspace_routes(&keyspace.name, &keyspace.routes)?;
        for route in &keyspace.routes {
            if !backends.contains_key(route.backend.as_str()) {
                return Err(format!(
                    "keyspace {:?} route {:?} backend {:?}, which is not a backend",
                    keyspace.name, route.key_prefix, route.backend
                ));
            }
        }
    }
    for c in &config.caches {
        if !keyspaces.contains_key(c.source.as_str())
            && !neighborhoods.contains_key(c.source.as_str())
        {
            return Err(format!(
                "cache {:?} source {:?}, which is not a keyspace or neighborhood",
                c.name, c.source
            ));
        }
    }
    for n in &config.neighborhoods {
        if !keyspaces.contains_key(n.source.as_str()) {
            return Err(format!(
                "neighborhood {:?} source {:?}, which is not a keyspace",
                n.name, n.source
            ));
        }
        validate_peer_ids(n)?;
    }
    validate_fabric_peer_ids(config)?;
    for f in &config.frontends {
        for mount in &f.mounts {
            if !keyspaces.contains_key(mount.source.as_str())
                && !caches.contains_key(mount.source.as_str())
                && !neighborhoods.contains_key(mount.source.as_str())
            {
                return Err(format!(
                    "frontend {:?} mount {:?} source {:?}, which is not a keyspace, cache, or neighborhood",
                    f.name, mount.public_prefix, mount.source
                ));
            }
        }
    }

    Ok(())
}

fn validate_keyspace_routes(keyspace: &str, routes: &[KeyspaceRoute]) -> Result<(), String> {
    let mut prefixes = HashSet::new();
    for route in routes {
        validate_prefix("keyspace", keyspace, "key_prefix", &route.key_prefix)?;
        validate_prefix(
            "keyspace",
            keyspace,
            "origin_prefix",
            &route.origin_prefix,
        )?;
        require_source("keyspace route", &route.key_prefix, &route.backend)?;
        if !prefixes.insert(route.key_prefix.as_str()) {
            return Err(format!(
                "keyspace {keyspace:?}: duplicate route key_prefix {:?}",
                route.key_prefix
            ));
        }
    }
    Ok(())
}

fn validate_frontend_mounts(frontend: &str, mounts: &[FrontendMount]) -> Result<(), String> {
    if mounts.is_empty() {
        return Err(format!("frontend {frontend:?}: at least one mount is required"));
    }

    let mut prefixes = HashSet::new();
    for mount in mounts {
        validate_prefix("frontend", frontend, "public_prefix", &mount.public_prefix)?;
        validate_prefix("frontend", frontend, "key_prefix", &mount.key_prefix)?;
        require_source("frontend mount", &mount.public_prefix, &mount.source)?;
        if !prefixes.insert(mount.public_prefix.as_str()) {
            return Err(format!(
                "frontend {frontend:?}: duplicate mount public_prefix {:?}",
                mount.public_prefix
            ));
        }
    }
    Ok(())
}

fn validate_prefix(kind: &str, name: &str, field: &str, prefix: &str) -> Result<(), String> {
    if prefix.is_empty() || !prefix.starts_with('/') {
        return Err(format!(
            "{kind} {name:?}: {field} must be an absolute path prefix"
        ));
    }
    Ok(())
}

fn validate_peer_ids(neighborhood: &super::schema::NeighborhoodSpec) -> Result<(), String> {
    let mut seen = HashSet::new();

    for peer in &neighborhood.peers {
        if !seen.insert(peer.id) {
            return Err(format!(
                "neighborhood {:?} peer {} is duplicated",
                neighborhood.name, peer.id,
            ));
        }
    }

    Ok(())
}

fn validate_fabric_peer_ids(config: &Config) -> Result<(), String> {
    let mut local_ids = Vec::new();
    for neighborhood in &config.neighborhoods {
        if let Some(local_node_id) = neighborhood.local_node_id {
            local_ids.push((neighborhood.name.as_str(), local_node_id));
        }
    }
    local_ids.sort_by_key(|(name, _)| *name);

    let process_local_id = match local_ids.first().map(|(_, id)| *id) {
        Some(id) => {
            if local_ids.iter().any(|(_, other)| *other != id) {
                let configured = local_ids
                    .iter()
                    .map(|(neighborhood_id, node_id)| format!("{neighborhood_id}:{node_id}"))
                    .collect::<Vec<_>>()
                    .join(", ");
                return Err(format!(
                    "neighborhoods declare different local_node_id values, but the storage fabric uses one process-wide peer id ({configured})"
                ));
            }
            Some(id)
        }
        None => None,
    };

    let mut peers_by_id: HashMap<u64, (&str, &PeerSpec)> = HashMap::new();
    for neighborhood in &config.neighborhoods {
        for peer in &neighborhood.peers {
            if Some(peer.id) == process_local_id {
                return Err(format!(
                    "neighborhood {:?} peer {} collides with process local_node_id {}",
                    neighborhood.name, peer.id, peer.id
                ));
            }
            if let Some((first_neighborhood, first_peer)) = peers_by_id.get(&peer.id) {
                if *first_peer != peer {
                    return Err(format!(
                        "peer id {} is declared with different peer data in neighborhoods {:?} and {:?}",
                        peer.id, first_neighborhood, neighborhood.name
                    ));
                }
            } else {
                peers_by_id.insert(peer.id, (neighborhood.name.as_str(), peer));
            }
        }
    }

    Ok(())
}

pub fn runtime_projection(config: &Config) -> Result<RuntimeGraph, String> {
    validate_binding_graph(config)?;

    let backends = by_id(&config.backends, |b| b.name.as_str());
    let keyspaces = by_id(&config.keyspaces, |k| k.name.as_str());
    let caches = by_id(&config.caches, |c| c.name.as_str());
    let neighborhoods = by_id(&config.neighborhoods, |n| n.name.as_str());

    let mut bindings = HashMap::new();

    for frontend in &config.frontends {
        let mut mounts = Vec::with_capacity(frontend.mounts.len());
        for mount in &frontend.mounts {
            let chain = resolve_mount_chain(mount, &keyspaces, &caches, &neighborhoods)?;
            let keyspace = keyspaces
                .get(chain.keyspace_id.as_str())
                .expect("binding graph validation checked keyspace target");
            let mut routes = keyspace
                .routes
                .iter()
                .map(|route| {
                    let backend = backends
                        .get(route.backend.as_str())
                        .expect("binding graph validation checked route backend");
                    ResolvedKeyspaceRoute {
                        key_prefix: route.key_prefix.clone(),
                        backend_id: route.backend.clone(),
                        origin_prefix: route.origin_prefix.clone(),
                        stripe_size: backend.stripe_size_bytes(),
                    }
                })
                .collect::<Vec<_>>();
            sort_by_longest_prefix(&mut routes, |route| route.key_prefix.as_str());
            mounts.push(ResolvedFrontendMount {
                public_prefix: mount.public_prefix.clone(),
                keyspace_id: chain.keyspace_id,
                key_prefix: mount.key_prefix.clone(),
                cache_id: chain.cache_id,
                neighborhood_id: chain.neighborhood_id,
                bypass_cache: chain.bypass_cache,
                routes,
            });
        }
        sort_by_longest_prefix(&mut mounts, |mount| mount.public_prefix.as_str());
        bindings.insert(
            frontend.name.clone(),
            ResolvedFrontendBinding {
                frontend_id: frontend.name.clone(),
                mounts,
            },
        );
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
                    fabric_peer_id: PeerId(peer.id),
                    spec: peer.clone(),
                })
                .collect();
            (
                neighborhood.name.clone(),
                RuntimeNeighborhood {
                    id: neighborhood.name.clone(),
                    keyspace_id: neighborhood.source.clone(),
                    p2p: RuntimeP2p {
                        fingers_per_node: neighborhood.fingers_per_node.unwrap_or(100).max(1),
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

pub fn frontend_backend_map(
    bindings: &HashMap<String, ResolvedFrontendBinding>,
) -> HashMap<String, Vec<String>> {
    bindings
        .iter()
        .map(|(id, binding)| {
            let mut backends = binding
                .mounts
                .iter()
                .flat_map(|mount| mount.routes.iter().map(|route| route.backend_id.clone()))
                .collect::<Vec<_>>();
            backends.sort();
            backends.dedup();
            (id.clone(), backends)
        })
        .collect()
}

struct ResolvedChain {
    keyspace_id: String,
    cache_id: Option<String>,
    neighborhood_id: Option<String>,
    bypass_cache: bool,
}

fn resolve_mount_chain(
    mount: &FrontendMount,
    keyspaces: &HashMap<&str, &super::schema::KeyspaceSpec>,
    caches: &HashMap<&str, &super::schema::CacheSpec>,
    neighborhoods: &HashMap<&str, &super::schema::NeighborhoodSpec>,
) -> Result<ResolvedChain, String> {
    if keyspaces.contains_key(mount.source.as_str()) {
        return Ok(ResolvedChain {
            keyspace_id: mount.source.clone(),
            cache_id: None,
            neighborhood_id: None,
            bypass_cache: true,
        });
    }

    if let Some(cache) = caches.get(mount.source.as_str()) {
        if let Some(neighborhood) = neighborhoods.get(cache.source.as_str()) {
            return Ok(ResolvedChain {
                keyspace_id: neighborhood.source.clone(),
                cache_id: Some(cache.name.clone()),
                neighborhood_id: Some(neighborhood.name.clone()),
                bypass_cache: false,
            });
        }
        return Ok(ResolvedChain {
            keyspace_id: cache.source.clone(),
            cache_id: Some(cache.name.clone()),
            neighborhood_id: None,
            bypass_cache: false,
        });
    }

    let neighborhood = neighborhoods
        .get(mount.source.as_str())
        .ok_or_else(|| format!("frontend mount source {:?} is unresolved", mount.source))?;
    Ok(ResolvedChain {
        keyspace_id: neighborhood.source.clone(),
        cache_id: None,
        neighborhood_id: Some(neighborhood.name.clone()),
        bypass_cache: false,
    })
}

fn sort_by_longest_prefix<T>(items: &mut [T], prefix: impl Fn(&T) -> &str) {
    items.sort_by(|a, b| {
        prefix(b)
            .len()
            .cmp(&prefix(a).len())
            .then_with(|| prefix(a).cmp(prefix(b)))
    });
}

fn prefix_suffix<'a>(path: &'a str, prefix: &str) -> Option<&'a str> {
    if prefix == "/" {
        return path.strip_prefix('/');
    }
    if path == prefix {
        return Some("");
    }
    let suffix = path.strip_prefix(prefix)?;
    if prefix.ends_with('/') || suffix.starts_with('/') {
        Some(suffix)
    } else {
        None
    }
}

fn join_prefix_suffix(prefix: &str, suffix: &str) -> String {
    if prefix == "/" {
        format!("/{suffix}")
    } else {
        format!("{prefix}{suffix}")
    }
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
        BackendSpec, CacheSpec, FrontendMount, FrontendSpec, HttpBackendConfig,
        HttpFrontendConfig, KeyspaceRoute, KeyspaceSpec, NeighborhoodSpec, PeerSpec,
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
            })),
        }
    }

    fn backend_with_stripe(id: &str, stripe_size: u64) -> BackendSpec {
        BackendSpec {
            name: id.to_string(),
            config: Some(backend_spec::Config::Http(HttpBackendConfig {
                url: "https://example.com".to_string(),
                stripe_size_bytes: Some(stripe_size),
                http_concurrency: Some(64),
            })),
        }
    }

    fn keyspace(id: &str, routes: &[(&str, &str, &str)]) -> KeyspaceSpec {
        KeyspaceSpec {
            name: id.to_string(),
            routes: routes
                .iter()
                .map(|(key_prefix, backend, origin_prefix)| KeyspaceRoute {
                    key_prefix: (*key_prefix).to_string(),
                    backend: (*backend).to_string(),
                    origin_prefix: (*origin_prefix).to_string(),
                })
                .collect(),
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
            fingers_per_node: Some(100),
            local_node_id: Some(1),
            local_tags: Vec::new(),
            routing_plan: None,
            peers: Vec::new(),
        }
    }

    fn frontend(id: &str, source: &str) -> FrontendSpec {
        FrontendSpec {
            name: id.to_string(),
            mounts: vec![FrontendMount {
                public_prefix: "/".to_string(),
                source: source.to_string(),
                key_prefix: "/".to_string(),
            }],
            config: Some(frontend_spec::Config::Http(HttpFrontendConfig {
                addr: format!("127.0.0.1:{}", 9000 + id.len()),
            })),
        }
    }

    fn frontend_with_mounts(id: &str, mounts: &[(&str, &str, &str)]) -> FrontendSpec {
        FrontendSpec {
            name: id.to_string(),
            mounts: mounts
                .iter()
                .map(|(public_prefix, source, key_prefix)| FrontendMount {
                    public_prefix: (*public_prefix).to_string(),
                    source: (*source).to_string(),
                    key_prefix: (*key_prefix).to_string(),
                })
                .collect(),
            config: Some(frontend_spec::Config::Http(HttpFrontendConfig {
                addr: format!("127.0.0.1:{}", 9000 + id.len()),
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
        cfg.keyspaces.push(keyspace("ks", &[("/", "b", "/")]));
        cfg.caches.push(cache("c-keyspace", "ks"));
        cfg.neighborhoods.push(neighborhood("n", "ks"));
        cfg.caches.push(cache("c-neighborhood", "n"));
        cfg.frontends.push(frontend("direct", "ks"));
        cfg.frontends.push(frontend("cache", "c-keyspace"));
        cfg.frontends.push(frontend("neighborhood", "n"));
        cfg.frontends.push(frontend("full", "c-neighborhood"));

        let graph = runtime_projection(&cfg).unwrap();

        assert_binding(&graph, "direct", "ks", None, None, true);
        assert_binding(&graph, "cache", "ks", Some("c-keyspace"), None, false);
        assert_binding(&graph, "neighborhood", "ks", None, Some("n"), false);
        assert_binding(
            &graph,
            "full",
            "ks",
            Some("c-neighborhood"),
            Some("n"),
            false,
        );
        assert_eq!(graph.caches.len(), 2);
        assert_eq!(graph.neighborhoods.len(), 1);
    }

    #[test]
    fn runtime_projection_resolves_multiple_backend_keyspace() {
        let mut cfg = Config::default();
        cfg.backends.push(backend_with_stripe("east", 1024));
        cfg.backends.push(backend_with_stripe("west", 2048));
        cfg.keyspaces.push(keyspace(
            "models",
            &[
                ("/llama/", "east", "/prod/llama/"),
                ("/mistral/", "west", "/models/mistral/"),
            ],
        ));
        cfg.frontends.push(frontend("f", "models"));

        let graph = runtime_projection(&cfg).unwrap();
        let mount = &graph.frontends["f"].mounts[0];

        assert_eq!(mount.routes.len(), 2);
        assert_eq!(mount.routes[0].backend_id, "west");
        assert_eq!(mount.routes[0].stripe_size, 2048);
        assert_eq!(mount.routes[1].backend_id, "east");
        assert_eq!(frontend_backend_map(&graph.frontends)["f"], vec!["east", "west"]);
    }

    #[test]
    fn runtime_projection_sorts_mounts_by_longest_prefix() {
        let mut cfg = Config::default();
        cfg.backends.push(backend("b"));
        cfg.keyspaces.push(keyspace("ks", &[("/", "b", "/")]));
        cfg.frontends.push(frontend_with_mounts(
            "f",
            &[("/", "ks", "/"), ("/private/", "ks", "/tenant/private/")],
        ));

        let graph = runtime_projection(&cfg).unwrap();
        let mounts = &graph.frontends["f"].mounts;

        assert_eq!(mounts[0].public_prefix, "/private/");
        assert_eq!(mounts[1].public_prefix, "/");
    }

    #[test]
    fn resolved_binding_maps_public_path_to_keyspace_and_origin() {
        let mut cfg = Config::default();
        cfg.backends.push(backend("b"));
        cfg.keyspaces.push(keyspace(
            "ks",
            &[("/tenant/private/", "b", "/bucket/private/")],
        ));
        cfg.frontends.push(frontend_with_mounts(
            "f",
            &[("/private/", "ks", "/tenant/private/")],
        ));

        let graph = runtime_projection(&cfg).unwrap();
        let resolved = graph.frontends["f"].resolve_path("/private/a/b.bin").unwrap();

        assert_eq!(resolved.keyspace_id, "ks");
        assert_eq!(resolved.key_object_id, "/tenant/private/a/b.bin");
        assert_eq!(resolved.backend_id, "b");
        assert_eq!(resolved.origin_object_id, "/bucket/private/a/b.bin");
    }

    #[test]
    fn resolved_binding_prefixes_match_path_boundaries() {
        let mut cfg = Config::default();
        cfg.backends.push(backend("b"));
        cfg.keyspaces.push(keyspace("ks", &[("/foo", "b", "/origin/foo")]));
        cfg.frontends.push(frontend_with_mounts("f", &[("/public", "ks", "/foo")]));

        let graph = runtime_projection(&cfg).unwrap();
        let binding = &graph.frontends["f"];

        assert!(binding.resolve_path("/public/a").is_some());
        assert!(binding.resolve_path("/public").is_some());
        assert!(binding.resolve_path("/publicity/a").is_none());
        assert!(binding.resolve_path("/publicabar").is_none());
    }

    #[test]
    fn runtime_projection_rejects_backend_sources() {
        let mut cfg = Config::default();
        cfg.backends.push(backend("b"));
        cfg.frontends.push(frontend("f", "b"));

        let err = runtime_projection(&cfg).unwrap_err();

        assert!(err.contains("not a keyspace, cache, or neighborhood"), "{err}");
    }

    #[test]
    fn runtime_projection_rejects_duplicate_mount_prefix() {
        let mut cfg = Config::default();
        cfg.backends.push(backend("b"));
        cfg.keyspaces.push(keyspace("ks", &[("/", "b", "/")]));
        cfg.frontends
            .push(frontend_with_mounts("f", &[("/", "ks", "/"), ("/", "ks", "/x/")]));

        let err = runtime_projection(&cfg).unwrap_err();

        assert!(err.contains("duplicate mount public_prefix"), "{err}");
    }

    #[test]
    fn runtime_projection_rejects_duplicate_route_prefix() {
        let mut cfg = Config::default();
        cfg.backends.push(backend("b"));
        cfg.keyspaces
            .push(keyspace("ks", &[("/", "b", "/"), ("/", "b", "/x/")]));
        cfg.frontends.push(frontend("f", "ks"));

        let err = runtime_projection(&cfg).unwrap_err();

        assert!(err.contains("duplicate route key_prefix"), "{err}");
    }

    #[test]
    fn runtime_projection_accepts_unique_rdma_peer_ids() {
        let mut cfg = Config::default();
        cfg.backends.push(backend("b"));
        cfg.keyspaces.push(keyspace("ks", &[("/", "b", "/")]));
        let mut n = neighborhood("n", "ks");
        n.peers.push(rdma_peer(7, "hex:00"));
        n.peers.push(rdma_peer(8, "hex:01"));
        cfg.neighborhoods.push(n);

        let graph = runtime_projection(&cfg).unwrap();
        let peers = &graph.neighborhoods["n"].peers;

        assert_eq!(peers.len(), 2);
        assert_eq!(peers[0].node_id, NodeId(7));
        assert_eq!(peers[1].node_id, NodeId(8));
        assert_eq!(peers[0].fabric_peer_id, PeerId(7));
        assert_eq!(peers[1].fabric_peer_id, PeerId(8));
    }

    #[test]
    fn runtime_projection_accepts_reused_peer_id_with_same_endpoint() {
        let mut cfg = Config::default();
        cfg.backends.push(backend("b"));
        cfg.keyspaces.push(keyspace("ks", &[("/", "b", "/")]));
        let mut a = neighborhood("n-a", "ks");
        a.peers.push(tcp_peer(7, "127.0.0.1:7"));
        let mut b = neighborhood("n-b", "ks");
        b.peers.push(tcp_peer(7, "127.0.0.1:7"));
        cfg.neighborhoods.push(a);
        cfg.neighborhoods.push(b);

        let graph = runtime_projection(&cfg).unwrap();

        assert_eq!(
            graph.neighborhoods["n-a"].peers[0].fabric_peer_id,
            PeerId(7)
        );
        assert_eq!(
            graph.neighborhoods["n-b"].peers[0].fabric_peer_id,
            PeerId(7)
        );
    }

    #[test]
    fn runtime_projection_rejects_reused_peer_id_with_different_peer_data() {
        let mut cfg = Config::default();
        cfg.backends.push(backend("b"));
        cfg.keyspaces.push(keyspace("ks", &[("/", "b", "/")]));
        let mut a = neighborhood("n-a", "ks");
        a.peers.push(tcp_peer(7, "127.0.0.1:7"));
        let mut b = neighborhood("n-b", "ks");
        b.peers.push(tcp_peer(7, "127.0.0.1:8"));
        cfg.neighborhoods.push(a);
        cfg.neighborhoods.push(b);

        let err = runtime_projection(&cfg).unwrap_err();

        assert!(err.contains("different peer data"), "{err}");
    }

    #[test]
    fn runtime_projection_accepts_same_local_node_id_across_neighborhoods() {
        let mut cfg = Config::default();
        cfg.backends.push(backend("b"));
        cfg.keyspaces.push(keyspace("ks", &[("/", "b", "/")]));
        let mut a = neighborhood("n-a", "ks");
        a.local_node_id = Some(7);
        let mut b = neighborhood("n-b", "ks");
        b.local_node_id = Some(7);
        cfg.neighborhoods.push(a);
        cfg.neighborhoods.push(b);

        runtime_projection(&cfg).unwrap();
    }

    #[test]
    fn runtime_projection_rejects_different_local_node_ids() {
        let mut cfg = Config::default();
        cfg.backends.push(backend("b"));
        cfg.keyspaces.push(keyspace("ks", &[("/", "b", "/")]));
        let mut a = neighborhood("n-a", "ks");
        a.local_node_id = Some(7);
        let mut b = neighborhood("n-b", "ks");
        b.local_node_id = Some(8);
        cfg.neighborhoods.push(a);
        cfg.neighborhoods.push(b);

        let err = runtime_projection(&cfg).unwrap_err();

        assert!(err.contains("different local_node_id values"), "{err}");
    }

    #[test]
    fn runtime_projection_rejects_peer_colliding_with_process_local_node_id() {
        let mut cfg = Config::default();
        cfg.backends.push(backend("b"));
        cfg.keyspaces.push(keyspace("ks", &[("/", "b", "/")]));
        let mut a = neighborhood("n-a", "ks");
        a.local_node_id = Some(7);
        let mut b = neighborhood("n-b", "ks");
        b.local_node_id = Some(7);
        b.peers.push(tcp_peer(7, "127.0.0.1:7"));
        cfg.neighborhoods.push(a);
        cfg.neighborhoods.push(b);

        let err = runtime_projection(&cfg).unwrap_err();

        assert!(err.contains("collides with process local_node_id"), "{err}");
    }

    #[test]
    fn runtime_projection_rejects_duplicate_peer_ids() {
        let mut cfg = Config::default();
        cfg.backends.push(backend("b"));
        cfg.keyspaces.push(keyspace("ks", &[("/", "b", "/")]));
        let mut n = neighborhood("n", "ks");
        n.peers.push(tcp_peer(7, "127.0.0.1:1"));
        n.peers.push(tcp_peer(7, "127.0.0.1:2"));
        cfg.neighborhoods.push(n);

        let err = runtime_projection(&cfg).unwrap_err();
        assert!(err.contains("peer 7 is duplicated"), "{err}");
    }

    #[test]
    fn runtime_projection_rejects_duplicate_rdma_peer_ids() {
        let mut cfg = Config::default();
        cfg.backends.push(backend("b"));
        cfg.keyspaces.push(keyspace("ks", &[("/", "b", "/")]));
        let mut n = neighborhood("n", "ks");
        n.peers.push(rdma_peer(7, "hex:00"));
        n.peers.push(rdma_peer(7, "hex:01"));
        cfg.neighborhoods.push(n);

        let err = runtime_projection(&cfg).unwrap_err();
        assert!(err.contains("peer 7 is duplicated"), "{err}");
    }

    fn assert_binding(
        graph: &RuntimeGraph,
        frontend_id: &str,
        keyspace_id: &str,
        cache_id: Option<&str>,
        neighborhood_id: Option<&str>,
        bypass_cache: bool,
    ) {
        let binding = graph.frontends.get(frontend_id).unwrap();
        let mount = &binding.mounts[0];
        assert_eq!(mount.keyspace_id, keyspace_id);
        assert_eq!(mount.cache_id.as_deref(), cache_id);
        assert_eq!(mount.neighborhood_id.as_deref(), neighborhood_id);
        assert_eq!(mount.bypass_cache, bypass_cache);
    }
}
