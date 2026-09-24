// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Full-snapshot control-plane subscription. HTTP and ProtoJSON files have the
//! same validation/publication boundary; a failed update retains the last value.
use crate::peer_identity::{FabricId, MAX_AUTHORITY_LEN, NodeId};
use crate::{crypto, handlers::Backend, http_client as http, uring};
use prost::Message;
#[path = "credentials.rs"]
pub mod credentials;
#[cfg(test)]
use std::sync::atomic::AtomicU64;
use std::{
    collections::{BTreeMap, BTreeSet},
    ffi::CString,
    io::{self, BufRead, Read, Write},
    net::SocketAddr,
    os::{
        fd::{AsFd, AsRawFd, FromRawFd, OwnedFd},
        unix::{
            ffi::OsStrExt,
            fs::{MetadataExt, OpenOptionsExt},
        },
    },
    path::{Path, PathBuf},
    sync::{Arc, Mutex, atomic::Ordering},
    time::{Duration, Instant},
};

pub mod proto {
    include!(concat!(env!("OUT_DIR"), "/racer.control.v1.rs"));
    include!(concat!(env!("OUT_DIR"), "/racer.control.v1.serde.rs"));
}
mod activation;
mod storage_policy;
pub use activation::Decision;
pub use storage_policy::{StoragePolicyStatus, StorageRequest, StorageResult};
const LIMIT: usize = 64 * 1024 * 1024;
const CONTROL_FIRST_BYTE_TIMEOUT: Duration = Duration::from_secs(35);
pub const MAX_SLOTS: u32 = 262144;
const MAX_CONFIG_WORK: u64 = 64 * 1024 * 1024;
fn invalid(message: impl Into<Box<dyn std::error::Error + Send + Sync>>) -> io::Error {
    io::Error::new(io::ErrorKind::InvalidData, message)
}
pub struct Trust {
    pub universe: [u8; 32],
    pub node: [u8; 32],
}
/// Editable wire input has no serving authority. Building consumes it and runs
/// the same identity, geometry, endpoint, and resource checks as subscription.
pub struct PreparedBuilder<'a> {
    trust: &'a Trust,
    snapshot: proto::Snapshot,
}
impl PreparedBuilder<'_> {
    pub fn snapshot_mut(&mut self) -> &mut proto::Snapshot {
        &mut self.snapshot
    }
    pub fn build(self) -> io::Result<Prepared> {
        self.trust.prepare(proto::Configuration {
            contents: Some(proto::configuration::Contents::Snapshot(self.snapshot)),
        })
    }
}
impl Trust {
    pub fn builder(&self, snapshot: proto::Snapshot) -> PreparedBuilder<'_> {
        PreparedBuilder {
            trust: self,
            snapshot,
        }
    }
    pub fn from_env() -> io::Result<Self> {
        fn identity(name: &str) -> io::Result<[u8; 32]> {
            let value = std::env::var(name).map_err(invalid)?;
            Ok(value.parse::<NodeId>()?.bytes())
        }
        Ok(Self {
            universe: identity("RACER_UNIVERSE")?,
            node: identity("RACER_NODE")?,
        })
    }
    pub fn prepare(&self, envelope: proto::Configuration) -> io::Result<Prepared> {
        #[cfg(test)]
        tests::probe_prepare()?;
        if envelope.encoded_len() > LIMIT {
            return Err(invalid("configuration byte budget exceeded"));
        }
        let Some(proto::configuration::Contents::Snapshot(config)) = envelope.contents else {
            return Err(invalid("missing configuration snapshot"));
        };
        if config.universe != self.universe || config.node != self.node || config.revision == 0 {
            return Err(invalid("configuration identity or revision mismatch"));
        }
        if config.volumes.len() > 64
            || config.peers.len() > 100000
            || config.member_catalogs.len() > 64
        {
            return Err(invalid("configuration object budget exceeded"));
        }
        if config.idle
            && (!config.volumes.is_empty()
                || !config.peers.is_empty()
                || !config.member_catalogs.is_empty())
        {
            return Err(invalid("idle configuration must have no volumes or peers"));
        }
        let mut work = 0u64;
        let mut records = config.peers.len();
        for v in &config.volumes {
            if let Some(t) = &v.topology {
                if t.slot_count > MAX_SLOTS {
                    return Err(invalid("slot geometry budget exceeded"));
                }
                work = work
                    .checked_add(t.local_slots.len() as u64 * 64)
                    .ok_or_else(|| invalid("work overflow"))?;
                records += t.local_slots.len() + t.neighbors.len();
            }
            records += v.peers.len() + v.peer_endpoints.as_ref().map_or(0, |s| s.peers.len());
        }
        if work > MAX_CONFIG_WORK || records > 2 * 1024 * 1024 {
            return Err(invalid("configuration preparation budget exceeded"));
        }
        let mut catalogs = Vec::new();
        for catalog in &config.member_catalogs {
            records += catalog.members.len();
            if catalog.members.len() > 100000 || records > 2 * 1024 * 1024 {
                return Err(invalid("membership catalog budget exceeded"));
            }
            let mut members = crate::http_auth::Members::new();
            for member in &catalog.members {
                let node: [u8; 32] = member
                    .node
                    .as_slice()
                    .try_into()
                    .map_err(|_| invalid("invalid member node"))?;
                if member.pod_uid.is_empty()
                    || member.pod_uid.len() > 253
                    || member.fabric.len() > 1024
                    || members
                        .insert(node, (member.pod_uid.clone(), member.fabric.clone()))
                        .is_some()
                {
                    return Err(invalid("invalid or duplicate member process"));
                }
            }
            catalogs.push(Arc::new(members));
        }
        let crypto = crypto::Snapshot::new(crypto::UniverseId::new(self.universe));
        let mut peers = BTreeMap::new();
        for peer in &config.peers {
            if peer.id.is_empty() || peer.pod_uid.is_empty() || peers.contains_key(&peer.id) {
                return Err(invalid("duplicate or empty peer ID or missing Pod UID"));
            }
            peers.insert(peer.id.clone(), http::Endpoint::parse(&peer.http_address)?);
        }
        let mut ids = BTreeSet::new();
        let mut sockets = BTreeSet::new();
        let mut volumes = Vec::new();
        for volume in &config.volumes {
            if !(1..=8).contains(&volume.max_candidate_attempts.unwrap_or(3)) {
                return Err(invalid("max_candidate_attempts must be in 1..=8"));
            }
            let cache_socket = crate::socket::UnixPath::new(&volume.cache_socket)?;
            let origin_socket = crate::socket::UnixPath::new(&volume.origin_socket)?;
            if !sockets.insert(cache_socket) || !sockets.insert(origin_socket) {
                return Err(invalid("duplicate cache or origin socket"));
            }
            if volume.id.is_empty() || !ids.insert(&volume.id) {
                return Err(invalid("duplicate or empty volume ID"));
            }
            let mut unique = BTreeSet::new();
            for peer in &volume.peers {
                if !peers.contains_key(peer) || !unique.insert(peer) {
                    return Err(invalid("unknown or duplicate volume peer"));
                }
            }
            let effective = effective_peers(&config, volume)?;
            let index = volume
                .member_catalog
                .ok_or_else(|| invalid("missing member catalog"))?;
            let members = catalogs
                .get(index as usize)
                .ok_or_else(|| invalid("unknown member catalog"))?
                .clone();
            // Endpoint hints cannot replace the catalog's process identity.
            for peer in config
                .peers
                .iter()
                .filter(|p| effective.contains_key(&p.id))
            {
                let node = crate::http_auth::identity_bytes(&peer.id)?;
                if !members
                    .get(&node)
                    .is_some_and(|(pod, fabric)| pod == &peer.pod_uid && fabric == &peer.fabric)
                {
                    return Err(invalid("endpoint process differs from membership"));
                }
            }
            let endpoints = effective
                .iter()
                .map(|(id, (address, _))| Ok((id.clone(), http::Endpoint::parse(address)?)))
                .collect::<io::Result<BTreeMap<_, _>>>()?;
            volumes.push(PreparedVolume {
                routing: Arc::new(crate::routing::Routing::new(&config.universe, volume)?),
                config: volume.clone(),
                cache_socket,
                backend: Backend::unix(&volume.origin_socket, &volume.id)?,
                peers: endpoints,
                effective,
                members,
            });
        }
        let eligibility = Eligibility::prepare(&config, &peers, &volumes);
        use sha2::Digest;
        let digest = crate::cache::peer_wire::hex(&sha2::Sha256::digest(config.encode_to_vec()));
        Ok(Prepared {
            config,
            crypto,
            peers,
            volumes,
            eligibility,
            digest,
        })
    }
}
pub struct PreparedVolume {
    routing: Arc<crate::routing::Routing>,
    config: proto::Volume,
    cache_socket: crate::socket::UnixPath,
    backend: Backend,
    peers: BTreeMap<String, http::Endpoint>,
    effective: BTreeMap<String, (String, String)>,
    members: Arc<crate::http_auth::Members>,
}
impl PreparedVolume {
    pub fn config(&self) -> &proto::Volume {
        &self.config
    }
    pub fn routing(&self) -> &Arc<crate::routing::Routing> {
        &self.routing
    }
    pub fn cache_socket(&self) -> crate::socket::UnixPath {
        self.cache_socket
    }
    pub fn backend(&self) -> &Backend {
        &self.backend
    }
    pub fn namespace(&self, universe: &[u8]) -> crate::cache::Namespace {
        crate::cache::Namespace::volume(
            universe,
            &self.config.id,
            self.config.cache_generation,
            self.backend.namespace(),
        )
    }
    pub fn peers(&self) -> &BTreeMap<String, http::Endpoint> {
        &self.peers
    }
}

fn effective_peers(
    config: &proto::Snapshot,
    volume: &proto::Volume,
) -> io::Result<BTreeMap<String, (String, String)>> {
    let global: BTreeMap<_, _> = config.peers.iter().map(|p| (p.id.as_str(), p)).collect();
    let mut result = BTreeMap::new();
    let scope = volume
        .peer_endpoints
        .as_ref()
        .ok_or_else(|| invalid("volume peer scope required"))?;
    for endpoint in &scope.peers {
        let peer = global
            .get(endpoint.peer.as_str())
            .ok_or_else(|| invalid("unknown scoped peer"))?;
        let url = if endpoint.http_address.is_empty() {
            &peer.http_address
        } else {
            &endpoint.http_address
        };
        if result
            .insert(peer.id.clone(), (url.clone(), peer.fabric.clone()))
            .is_some()
        {
            return Err(invalid("duplicate scoped peer"));
        }
    }
    if volume.peers.iter().any(|p| !result.contains_key(p)) {
        return Err(invalid("outgoing peer missing from volume scope"));
    }
    Ok(result)
}
/// A validated, immutable generation. All serving and capability APIs borrow
/// the same authoritative inputs. Use `Trust::builder` to edit unvalidated input.
///
/// ```compile_fail
/// use racer_dataplane::control::Prepared;
/// fn substitute(prepared: &mut Prepared) {
///     prepared.config_snapshot().peers.clear();
/// }
/// ```
///
/// ```compile_fail
/// use racer_dataplane::control::Prepared;
/// fn substitute(prepared: &mut Prepared) {
///     prepared.volumes()[0].config().peers.clear();
/// }
/// ```
pub struct Prepared {
    config: proto::Snapshot,
    crypto: crypto::Snapshot,
    peers: BTreeMap<String, http::Endpoint>,
    volumes: Vec<PreparedVolume>,
    eligibility: Eligibility,
    digest: String,
}

struct EligibleRecord {
    node: NodeId,
    peer_index: usize,
}
struct Eligibility {
    local: NodeId,
    fabric: Option<FabricId>,
    peers: BTreeMap<String, EligibleRecord>,
    volumes: BTreeMap<String, BTreeMap<String, EligibleRecord>>,
}
impl Eligibility {
    fn prepare(
        config: &proto::Snapshot,
        endpoints: &BTreeMap<String, http::Endpoint>,
        volumes: &[PreparedVolume],
    ) -> Self {
        let local = NodeId::from_bytes(&config.node).expect("validated snapshot node");
        let fabric = FabricId::new(&config.fabric).ok();
        // Case variants are the same node. Ambiguous duplicate node identities
        // remain HTTP-only rather than choosing an endpoint by iteration order.
        let mut counts = BTreeMap::new();
        for peer in &config.peers {
            if let Ok(node) = peer.id.parse::<NodeId>() {
                *counts.entry(node).or_insert(0usize) += 1;
            }
        }
        let records = |endpoints: &BTreeMap<String, http::Endpoint>| {
            let mut peers = BTreeMap::new();
            if let Some(fabric) = &fabric {
                for (peer_index, peer) in config.peers.iter().enumerate() {
                    let Ok(node) = peer.id.parse::<NodeId>() else {
                        continue;
                    };
                    let Some(backend) = endpoints.get(&peer.id) else {
                        continue;
                    };
                    let Some(address) = backend.address().tcp() else {
                        continue;
                    };
                    if node == local
                        || counts[&node] != 1
                        || peer.fabric != fabric.as_str()
                        || address.port() == 0
                        || address.ip().is_unspecified()
                        || address.ip().is_multicast()
                        || address.ip() == std::net::Ipv4Addr::BROADCAST
                        || backend.host().len() > MAX_AUTHORITY_LEN
                    {
                        continue;
                    }
                    peers.insert(peer.id.clone(), EligibleRecord { node, peer_index });
                }
            }
            peers
        };
        let peers = records(endpoints);
        let volumes = volumes
            .iter()
            .map(|v| (v.config.id.clone(), records(&v.peers)))
            .collect();
        Self {
            local,
            fabric,
            peers,
            volumes,
        }
    }
}

/// Configuration permission to attempt RDMA, not proof of remote authentication
/// or physical registration. Obtain this only from the worker's **activated**
/// `Prepared` generation; `Updates::latest` can still be staging. The borrow pins
/// the authorization inputs, and does not authorize a later generation.
///
/// ```compile_fail
/// use racer_dataplane::control::EligiblePeer;
/// let peer = EligiblePeer {};
/// ```
pub struct EligiblePeer<'a> {
    prepared: &'a Prepared,
    id: &'a str,
    record: &'a EligibleRecord,
    endpoint: &'a http::Endpoint,
}
impl<'a> EligiblePeer<'a> {
    /// Configured ID spelling (volume lists use exact strings).
    pub fn id(&self) -> &'a str {
        self.id
    }
    pub fn node(&self) -> NodeId {
        self.record.node
    }
    pub fn pod_uid(&self) -> &str {
        &self.prepared.config.peers[self.record.peer_index].pod_uid
    }
    pub fn local_node(&self) -> NodeId {
        self.prepared.local_node()
    }
    pub fn fabric(&self) -> &'a FabricId {
        self.prepared.fabric().unwrap()
    }
    /// Reuse the numeric endpoint and its canonical IP Host authority.
    pub fn endpoint(&self) -> &'a http::Endpoint {
        self.endpoint
    }
    pub fn config_snapshot(&self) -> &'a proto::Snapshot {
        self.prepared.config_snapshot()
    }
    pub fn crypto_snapshot(&self) -> &'a crypto::Snapshot {
        self.prepared.crypto_snapshot()
    }
}
impl Prepared {
    pub(crate) fn authentication(&self, volume: &str) -> io::Result<crate::http_auth::Policy> {
        let volume = self
            .volumes
            .iter()
            .find(|v| v.config.id == volume)
            .ok_or_else(|| invalid("unknown membership volume"))?;
        Ok(crate::http_auth::Policy {
            universe: self.crypto.universe().bytes(),
            node: self.local_node().bytes(),
            members: volume.members.clone(),
        })
    }
    pub(crate) fn authorize_member(
        &self,
        volume: &str,
        identity: &crate::tls::PeerIdentity,
    ) -> io::Result<()> {
        self.authentication(volume)?.authorize(identity)
    }
    pub(crate) fn rdma_member(&self, volume: &str, node: NodeId) -> bool {
        node != self.local_node()
            && self.fabric().is_some_and(|fabric| {
                self.volumes
                    .iter()
                    .find(|v| v.config.id == volume)
                    .and_then(|v| v.members.get(&node.bytes()))
                    .is_some_and(|(_, f)| f == fabric.as_str())
            })
    }
    fn validate_successor(&self, old: &Self) -> io::Result<()> {
        for volume in &self.volumes {
            if let Some(previous) = old.volumes.iter().find(|v| v.config.id == volume.config.id) {
                let a = &previous.routing.geometry;
                let b = &volume.routing.geometry;
                if a.slot_count() != b.slot_count()
                    || b.epoch() < a.epoch()
                    || (a.epoch() == b.epoch()
                        && (previous.config.topology != volume.config.topology
                            || previous.config.origin_socket != volume.config.origin_socket
                            || previous.config.peers != volume.config.peers
                            || previous.effective != volume.effective))
                {
                    return Err(invalid(
                        "routing changes require a newer epoch and fixed slot count",
                    ));
                }
            }
        }
        Ok(())
    }
    pub fn peers(&self) -> &BTreeMap<String, http::Endpoint> {
        &self.peers
    }
    pub fn volumes(&self) -> &[PreparedVolume] {
        &self.volumes
    }
    pub fn peer_identity(&self, id: &str) -> io::Result<crate::tls::PeerIdentity> {
        let peer = self
            .config_snapshot()
            .peers
            .iter()
            .find(|p| p.id == id)
            .ok_or_else(|| invalid("unknown peer identity"))?;
        let universe: String = self
            .config_snapshot()
            .universe
            .iter()
            .map(|b| format!("{b:02x}"))
            .collect();
        crate::tls::PeerIdentity::new(&universe, &peer.id, &peer.pod_uid)
    }
    pub fn routing_for_volume(&self, volume: &str) -> Option<&Arc<crate::routing::Routing>> {
        self.volumes
            .iter()
            .find(|v| v.config.id == volume)
            .map(|v| &v.routing)
    }
    pub fn local_node(&self) -> NodeId {
        self.eligibility.local
    }
    /// None disables RDMA, including malformed or over-limit fabric strings.
    pub fn fabric(&self) -> Option<&FabricId> {
        self.eligibility.fabric.as_ref()
    }
    /// Original, validated inputs used for eligibility and negotiation.
    pub fn config_snapshot(&self) -> &proto::Snapshot {
        &self.config
    }
    pub fn crypto_snapshot(&self) -> &crypto::Snapshot {
        &self.crypto
    }
    pub fn eligible_peer(&self, id: &str) -> Option<EligiblePeer<'_>> {
        let (id, record) = self.eligibility.peers.get_key_value(id)?;
        Some(EligiblePeer {
            prepared: self,
            id,
            record,
            endpoint: &self.peers[id],
        })
    }
    /// Lookup an incoming authenticated node claim in configured direct peers.
    /// This lookup does not itself authenticate that claim.
    pub fn eligible_node(&self, node: NodeId) -> Option<EligiblePeer<'_>> {
        self.eligible_peers().find(|peer| peer.node() == node)
    }
    pub fn eligible_peers(&self) -> impl Iterator<Item = EligiblePeer<'_>> {
        self.eligibility
            .peers
            .iter()
            .map(|(id, record)| EligiblePeer {
                prepared: self,
                id,
                record,
                endpoint: &self.peers[id],
            })
    }
    /// Eligible subset in the volume's configured order; unknown volume is empty.
    pub fn eligible_peers_for_volume<'a>(
        &'a self,
        volume: &str,
    ) -> impl Iterator<Item = EligiblePeer<'a>> {
        self.config_snapshot()
            .volumes
            .iter()
            .find(|v| v.id == volume)
            .into_iter()
            .flat_map(|v| &v.peers)
            .filter_map(move |id| self.eligible_direct_peer_for_volume(volume, id))
    }
    pub fn eligible_peer_for_volume(&self, volume: &str, id: &str) -> Option<EligiblePeer<'_>> {
        let volume = self
            .config_snapshot()
            .volumes
            .iter()
            .find(|v| v.id == volume)?;
        volume
            .peers
            .iter()
            .any(|p| p == id)
            .then(|| self.eligible_direct_peer_for_volume(&volume.id, id))
            .flatten()
    }
    fn eligible_direct_peer_for_volume(&self, volume: &str, id: &str) -> Option<EligiblePeer<'_>> {
        let (id, record) = self.eligibility.volumes.get(volume)?.get_key_value(id)?;
        let volume = self.volumes.iter().find(|v| v.config.id == volume)?;
        Some(EligiblePeer {
            prepared: self,
            id,
            record,
            endpoint: &volume.peers[id],
        })
    }
    pub fn eligible_node_for_volume(&self, volume: &str, node: NodeId) -> Option<EligiblePeer<'_>> {
        let peers = self.eligibility.volumes.get(volume)?;
        let id = peers.iter().find(|(_, p)| p.node == node)?.0;
        self.eligible_direct_peer_for_volume(volume, id)
    }
    /// Initial topology next hop, with transport eligibility checked afterwards.
    pub fn select_eligible_peer(&self, volume: &str, target: &str) -> Option<EligiblePeer<'_>> {
        let volume = self.volumes.iter().find(|v| v.config.id == volume)?;
        let key = crate::cache::PeerDescriptor::metadata(target)
            .key(volume.namespace(&self.config_snapshot().universe))
            .ok()?;
        let (id, _) = volume
            .routing
            .next(&volume.routing.start_key(&key))
            .ok()??;
        self.eligible_peer_for_volume(&volume.config.id, &id)
    }
}

/// Publication is a complete Arc swap. Workers never observe partial lists.
#[derive(Default)]
pub struct Updates {
    lifecycle: std::sync::OnceLock<Arc<crate::lifecycle::Lifecycle>>,
    storage: Mutex<storage_policy::State>,
    boot: Mutex<Option<[u8; 32]>>,
    #[cfg(test)]
    subscription_probe: Arc<tests::Probe>,
    wakes: Mutex<Vec<Arc<uring::Wake>>>,
    coordinator: Mutex<activation::Coordinator>,
    last_error: Mutex<Option<String>>,
    credentials: Mutex<Option<Arc<credentials::Provider>>>,
    #[cfg(test)]
    before_active_publication: Mutex<Option<Arc<tests::activation_tests::ActivationPause>>>,
    #[cfg(test)]
    before_candidate_replacement: Mutex<Option<Arc<tests::activation_tests::ActivationPause>>>,
}
impl Updates {
    pub(crate) fn boot(&self) -> io::Result<[u8; 32]> {
        let mut boot = self.boot.lock().unwrap();
        if let Some(value) = *boot {
            return Ok(value);
        }
        let mut value = [0u8; 32];
        crate::environment::random(&mut value).map_err(|e| invalid(e.to_string()))?;
        *boot = Some(value);
        Ok(value)
    }
    pub fn credentials(&self) -> Option<Arc<credentials::Provider>> {
        self.credentials.lock().unwrap().clone()
    }
    pub fn set_credentials(&self, provider: Arc<credentials::Provider>) {
        *self.credentials.lock().unwrap() = Some(provider);
        self.wake_all();
    }
    /// Last epoch activated by all workers; pending/rejected updates retain it.
    pub fn applied_epoch(&self) -> u64 {
        self.coordinator.lock().unwrap().applied_epoch()
    }
    pub fn latest(&self, revision: u64) -> Option<Arc<Prepared>> {
        self.coordinator.lock().unwrap().latest(revision)
    }
    pub fn active(&self) -> Option<Arc<Prepared>> {
        self.coordinator.lock().unwrap().active()
    }
    pub fn status(&self) -> serde_json::Value {
        let mut status = self.coordinator.lock().unwrap().status();
        status["storage"] = self.storage_policy_status().json();
        status["lastError"] = serde_json::json!(*self.last_error.lock().unwrap());
        status["tls"] = serde_json::json!(self.credentials().map(|p| p.status()));
        status
    }

    pub fn subscribe(&self, wake: Arc<uring::Wake>) {
        self.wakes.lock().unwrap().push(wake);
        self.coordinator.lock().unwrap().subscribe();
    }
    fn wake_all(&self) {
        for wake in self.wakes.lock().unwrap().iter() {
            crate::workers::Wake::wake(&**wake);
        }
    }
    pub fn staged(&self, revision: u64, worker: usize, success: bool) {
        self.coordinator
            .lock()
            .unwrap()
            .staged(revision, worker, success);
        self.wake_all();
    }
    pub fn decision(&self, revision: u64) -> Decision {
        self.coordinator.lock().unwrap().decision(revision)
    }
    pub fn activated(&self, revision: u64, worker: usize) {
        self.coordinator
            .lock()
            .unwrap()
            .activated(revision, worker, || {
                #[cfg(test)]
                if let Some(pause) = self.before_active_publication.lock().unwrap().take() {
                    pause.wait();
                }
            });
        self.wake_all();
    }
    pub fn retired(&self, revision: u64, worker: usize) {
        self.coordinator.lock().unwrap().retired(revision, worker);
    }
    pub fn set_lifecycle(&self, lifecycle: Arc<crate::lifecycle::Lifecycle>) {
        assert!(self.lifecycle.set(lifecycle).is_ok());
    }
    pub fn publish(&self, next: Prepared) -> io::Result<()> {
        self.apply_desired(next)
    }
    pub fn apply_desired(&self, next: Prepared) -> io::Result<()> {
        self.coordinator
            .lock()
            .unwrap()
            .desired(next, || self.before_replacement())?;
        self.wake_all();
        Ok(())
    }
    fn before_replacement(&self) {
        #[cfg(test)]
        if let Some(pause) = self.before_candidate_replacement.lock().unwrap().take() {
            pause.wait();
        }
    }
}
#[derive(Clone)]
pub enum Source {
    Http {
        address: SocketAddr,
        host: String,
        target: String,
    },
    File(PathBuf),
}

impl Source {
    pub fn parse(value: &str) -> io::Result<Self> {
        if value.starts_with("https://") {
            let raw = &value[8..];
            let end = raw.find(['/', '?', '#']).unwrap_or(raw.len());
            let url = url::Url::parse(value).map_err(invalid)?;
            if !url.username().is_empty() || url.password().is_some() {
                return Err(invalid("control URL credentials forbidden"));
            }
            let addresses = url.socket_addrs(|| Some(8443)).map_err(invalid)?;
            let address = *addresses
                .first()
                .ok_or_else(|| invalid("control host has no addresses"))?;
            let suffix = &raw[end..];
            if suffix.contains('#') {
                return Err(invalid("invalid control URL"));
            }
            let target = if suffix.starts_with('/') {
                suffix.to_owned()
            } else {
                format!("/{suffix}")
            };
            http::Request::new(&target, &[])?;
            Ok(Self::Http {
                address,
                host: raw[..end].to_owned(),
                target,
            })
        } else if value.starts_with("file://") {
            Ok(Self::File(
                url::Url::parse(value)
                    .map_err(invalid)?
                    .to_file_path()
                    .map_err(|_| invalid("expected local file URL"))?,
            ))
        } else if value.is_empty() || value.contains("://") {
            Err(invalid("expected https:// URL or file path"))
        } else {
            Ok(Self::File(PathBuf::from(value)))
        }
    }
    pub fn from_env() -> io::Result<Self> {
        Self::parse(&std::env::var("RACER_CONTROL_PLANE_URL").map_err(invalid)?)
    }
}

/// One bounded control worker owns receive/decode/verification/preparation work. It
/// publishes at most one prepared replacement; payload buffers are never used.
pub struct Subscriber {
    stop: Arc<std::sync::atomic::AtomicBool>,
    thread: Option<std::thread::JoinHandle<()>>,
}
#[derive(Default)]
struct Rejection {
    revision: u64,
    digest: String,
}
impl Rejection {
    fn check_revision(&self, updates: &Updates, revision: u64) -> io::Result<()> {
        let a = updates.coordinator.lock().unwrap();
        // A duplicate accepted candidate remains valid while a newer candidate
        // is rejected. Nothing older may reset rejection feedback.
        if revision < a.revision() || (revision < self.revision && revision != a.revision()) {
            return Err(invalid("stale control command revision"));
        }
        Ok(())
    }
    fn record(&mut self, revision: u64, digest: String) {
        if revision >= self.revision {
            self.revision = revision;
            self.digest = digest;
        }
    }
    fn accepted(&mut self, revision: u64) {
        if revision >= self.revision {
            self.revision = 0;
            self.digest.clear();
        }
    }
}

fn pin_pod(pinned: &mut String, command: &proto::DesiredState) -> io::Result<()> {
    if command.pod_uid.is_empty() {
        return Err(invalid("control command missing Pod identity"));
    } else if pinned.is_empty() {
        *pinned = command.pod_uid.clone();
    } else if *pinned != command.pod_uid {
        return Err(invalid("control Pod identity mismatch"));
    }
    Ok(())
}

fn desired_headers(
    updates: &Updates,
    cursor: &str,
    boot: &str,
    rejection: &Rejection,
) -> Vec<(&'static str, String)> {
    let coordinator = updates.coordinator.lock().unwrap();
    let active = coordinator.active();
    let local_state = coordinator.local_state();
    drop(coordinator);
    let applied_digest = active
        .as_ref()
        .map(|p| p.digest.clone())
        .unwrap_or_default();
    let mut headers = vec![
        ("X-Racer-Boot", boot.into()),
        ("X-Racer-Profile", "1".into()),
        ("X-Racer-Cursor", cursor.into()),
        (
            "X-Racer-Applied-Revision",
            active
                .as_ref()
                .map_or(0, |p| p.config_snapshot().revision)
                .to_string(),
        ),
        ("X-Racer-Applied-Digest", applied_digest),
        ("X-Racer-Rejected-Revision", rejection.revision.to_string()),
        (
            "X-Racer-Local-State",
            if rejection.revision > 0 {
                "failed"
            } else {
                local_state
            }
            .into(),
        ),
        (
            "X-Racer-Worker-Healthy",
            if updates.lifecycle.get().is_some_and(|life| life.healthy()) {
                "1"
            } else {
                "0"
            }
            .into(),
        ),
    ];
    headers.extend(updates.storage_headers());
    headers
}

impl Subscriber {
    pub fn start(source: Source, trust: Arc<Trust>, updates: Arc<Updates>) -> io::Result<Self> {
        Self::start_with_first_byte_timeout(source, trust, updates, CONTROL_FIRST_BYTE_TIMEOUT)
    }
    fn start_with_first_byte_timeout(
        source: Source,
        trust: Arc<Trust>,
        updates: Arc<Updates>,
        first_byte_timeout: Duration,
    ) -> io::Result<Self> {
        if let Source::Http { target, .. } = &source
            && target.split('?').next() != Some("/v1/config")
        {
            return Err(invalid("control subscription requires /v1/config"));
        }
        let coordinated = matches!(&source, Source::Http { .. });
        let credentials = updates.credentials();
        if coordinated && credentials.is_none() {
            return Err(invalid("HTTPS control requires enrolled node credentials"));
        }
        let boot = updates.boot()?;
        let hex = |bytes: &[u8]| bytes.iter().map(|b| format!("{b:02x}")).collect::<String>();
        let boot_hex = hex(&boot);
        let stop = Arc::new(std::sync::atomic::AtomicBool::new(false));
        let stopping = stop.clone();
        let thread = std::thread::Builder::new()
            .name("racer-control".into())
            .spawn(move || {
                #[cfg(test)]
                tests::PROBE.with_borrow_mut(|p| *p = Some(updates.subscription_probe.clone()));
                let mut accepted = AcceptedInput::default();
                let mut etag = None;
                let mut failures = 0u32;
                let mut digest = String::new();
                let mut pod_uid = credentials
                    .as_ref()
                    .map(|p| p.identity().pod_uid.clone())
                    .unwrap_or_default();
                let mut rejection = Rejection::default();
                let mut cursor = String::new();
                let mut random = u64::from_le_bytes(boot[..8].try_into().unwrap()).max(1);
                let mut transport = match &source {
                    Source::Http {
                        address,
                        host,
                        target,
                    } => Some(ControlTransport {
                        address: *address,
                        host: host.clone(),
                        target: target.clone(),
                        provider: credentials.as_ref().unwrap().clone(),
                        idle: None,
                        // Spread periodic reconnects across independently booted nodes.
                        lifetime: Duration::from_secs(240 + random % 61),
                    }),
                    Source::File(_) => None,
                };
                while !stopping.load(Ordering::Relaxed) {
                    #[cfg(test)]
                    tests::probe(|p| {
                        p.polls.fetch_add(1, Ordering::SeqCst);
                    });
                    let result = (|| {
                        let mut content_key = None;
                        let (envelope, next_etag) = match &source {
                            Source::File(path) => {
                                let Some(bytes) = accepted.file(path)? else {
                                    return Ok(());
                                };
                                content_key = Some(ContentKey::new(&bytes));
                                (Some(serde_json::from_slice(&bytes).map_err(invalid)?), None)
                            }
                            Source::Http { .. } => {
                                let headers =
                                    desired_headers(&updates, &cursor, &boot_hex, &rejection);
                                let provider = credentials.as_ref().unwrap();
                                let credential_headers = provider.headers();
                                let credential_revision = provider.current().revision;
                                let mut checkpoint = || {
                                    if stopping.load(Ordering::Relaxed) {
                                        return Err(io::Error::other("subscription stopped"));
                                    }
                                    if headers
                                        != desired_headers(&updates, &cursor, &boot_hex, &rejection)
                                        || credential_revision != provider.current().revision
                                        || credential_headers != provider.headers()
                                    {
                                        // read_exact retries Interrupted internally; cancellation
                                        // must escape the framing reader and discard this socket.
                                        return Err(io::Error::new(
                                            io::ErrorKind::ConnectionAborted,
                                            "local report changed",
                                        ));
                                    }
                                    Ok(())
                                };
                                let (body, next_etag) =
                                    transport.as_mut().unwrap().fetch_with_first_byte_timeout(
                                        etag.as_deref(),
                                        &headers,
                                        &mut checkpoint,
                                        first_byte_timeout,
                                    )?;
                                let envelope = match body {
                                    None => None,
                                    Some(body) => {
                                        let command = proto::DesiredState::decode(body.as_slice())
                                            .map_err(invalid)?;
                                        if command.universe != trust.universe
                                            || command.node != trust.node
                                            || command.incarnation != boot
                                            || command.profile != 1
                                        {
                                            return Err(invalid(
                                                "control command identity/profile mismatch",
                                            ));
                                        }
                                        pin_pod(&mut pod_uid, &command)?;
                                        if command.cursor.is_empty()
                                            || command.cursor.len() > 1024
                                            || !command.cursor.bytes().all(|b| b.is_ascii_graphic())
                                        {
                                            return Err(invalid("invalid desired-state cursor"));
                                        }
                                        // Receipt is independent of local validation and application.
                                        // A rejected snapshot must not trigger an immediate resend loop.
                                        cursor = command.cursor.clone();
                                        updates.control_observed();
                                        updates.receive_desired_storage(&command);
                                        rejection.check_revision(&updates, command.revision)?;
                                        rejection.record(
                                            command.revision,
                                            hex(&command.snapshot_digest),
                                        );
                                        let envelope =
                                            command.configuration.clone().ok_or_else(|| {
                                                invalid("desired state missing configuration")
                                            })?;
                                        use sha2::Digest;
                                        let raw = match envelope.contents.as_ref() {
                                            Some(proto::configuration::Contents::Snapshot(s)) => {
                                                s.encode_to_vec()
                                            }
                                            None => return Err(invalid("missing snapshot")),
                                        };
                                        let hash = sha2::Sha256::digest(&raw);
                                        if hash.as_slice() != command.snapshot_digest {
                                            return Err(invalid("candidate digest mismatch"));
                                        }
                                        if proto::Snapshot::decode(raw.as_slice())
                                            .map_err(invalid)?
                                            .revision
                                            != command.revision
                                        {
                                            return Err(invalid("candidate revision mismatch"));
                                        }
                                        if digest != hex(&hash) {
                                            let prepared = match trust.prepare(envelope) {
                                                Ok(p) => p,
                                                Err(e) => {
                                                    rejection.record(command.revision, hex(&hash));
                                                    return Err(e);
                                                }
                                            };
                                            if prepared.config.revision != command.revision {
                                                return Err(invalid("candidate revision mismatch"));
                                            }
                                            if let Err(error) = updates.apply_desired(prepared) {
                                                rejection.record(command.revision, hex(&hash));
                                                return Err(error);
                                            }
                                        }
                                        digest = hex(&hash);
                                        rejection.accepted(command.revision);
                                        None
                                    }
                                };
                                (envelope, next_etag)
                            }
                        };
                        if let Some(envelope) = envelope {
                            let prepared = trust.prepare(envelope)?;
                            updates.publish(prepared)?;
                            accepted.accept(content_key.unwrap());
                        }
                        if next_etag.is_some() {
                            etag = next_etag;
                        }
                        if coordinated {
                            updates.control_observed();
                        }
                        Ok::<_, io::Error>(())
                    })();
                    match result {
                        Ok(()) => {
                            failures = 0;
                            *updates.last_error.lock().unwrap() = None;
                        }
                        Err(error) => {
                            if let Some(transport) = &mut transport {
                                transport.idle = None;
                            }
                            if error.kind() == io::ErrorKind::ConnectionAborted {
                                failures = 0;
                            } else {
                                failures = (failures + 1).min(6);
                                *updates.last_error.lock().unwrap() = Some(error.to_string());
                            }
                        }
                    }
                    let delay = if coordinated && failures == 0 {
                        Duration::ZERO
                    } else {
                        retry_delay(failures, &mut random)
                    };
                    let until = Instant::now() + delay;
                    while !stopping.load(Ordering::Relaxed) && Instant::now() < until {
                        std::thread::park_timeout(
                            until
                                .saturating_duration_since(Instant::now())
                                .min(Duration::from_millis(100)),
                        );
                    }
                }
            })?;
        Ok(Self {
            stop,
            thread: Some(thread),
        })
    }
}
impl Drop for Subscriber {
    fn drop(&mut self) {
        self.stop.store(true, Ordering::Relaxed);
        if let Some(thread) = self.thread.take() {
            thread.thread().unpark();
            let _ = thread.join();
        }
    }
}

// Follow the configured path on every poll, rather than watching an inode that
// may have been replaced (including a projected Secret's ..data symlink).
#[derive(Clone, Debug, PartialEq, Eq)]
struct FileVersion {
    dev: u64,
    ino: u64,
    len: u64,
    modified: (i64, i64),
    changed: (i64, i64),
}
impl FileVersion {
    fn new(meta: std::fs::Metadata) -> io::Result<Self> {
        if !meta.is_file() {
            return Err(invalid("configuration path must name a regular file"));
        }
        Ok(Self {
            dev: meta.dev(),
            ino: meta.ino(),
            len: meta.len(),
            modified: (meta.mtime(), meta.mtime_nsec()),
            changed: (meta.ctime(), meta.ctime_nsec()),
        })
    }
}

#[derive(PartialEq, Eq)]
struct ContentKey(blake3::Hash);
impl ContentKey {
    fn new(bytes: &[u8]) -> Self {
        Self(blake3::hash(bytes))
    }
}

/// Cache only successfully published input.
/// Failed decode, preparation or publication is never cached.
#[derive(Default)]
struct AcceptedInput {
    content: Option<ContentKey>,
    file: Option<FileVersion>,
    pending_file: Option<FileVersion>,
}
impl AcceptedInput {
    fn matches(&self, key: &ContentKey) -> bool {
        self.content.as_ref() == Some(key)
    }

    fn accept(&mut self, key: ContentKey) {
        self.content = Some(key);
        self.file = self.pending_file.take();
    }

    fn file(&mut self, path: &Path) -> io::Result<Option<Vec<u8>>> {
        self.pending_file = None;
        let version = FileVersion::new(std::fs::metadata(path)?)?;
        if self.file.as_ref() == Some(&version) {
            return Ok(None);
        }
        // Do not block opening a FIFO if the path changes after the stat above.
        let mut file = std::fs::OpenOptions::new()
            .read(true)
            .custom_flags(libc::O_NONBLOCK)
            .open(path)?;
        let before = FileVersion::new(file.metadata()?)?;
        let mut bytes = Vec::new();
        #[cfg(test)]
        tests::probe(|p| {
            p.reads.fetch_add(1, Ordering::SeqCst);
        });
        (&mut file)
            .take((LIMIT + 1) as u64)
            .read_to_end(&mut bytes)?;
        if bytes.len() > LIMIT {
            return Err(invalid("configuration file exceeds budget"));
        }
        if before != FileVersion::new(file.metadata()?)?
            || before != FileVersion::new(std::fs::metadata(path)?)?
        {
            return Err(invalid("configuration file changed while loading"));
        }
        // A rewrite/replacement of the same accepted bytes needs neither decode
        // nor preparation, but records the new inode/timestamps for future polls.
        if self.matches(&ContentKey::new(&bytes)) {
            self.file = Some(before);
            return Ok(None);
        }
        self.pending_file = Some(before);
        Ok(Some(bytes))
    }
}

fn retry_delay(failures: u32, random: &mut u64) -> Duration {
    if failures == 0 {
        return Duration::from_millis(250);
    }
    // Bounded 80–100% jitter, seeded by the independent subscription boot nonce.
    *random ^= *random << 13;
    *random ^= *random >> 7;
    *random ^= *random << 17;
    let cap = (250u64 << failures.min(6)).min(4000);
    Duration::from_millis(cap * 4 / 5 + *random % (cap / 5 + 1))
}

struct Receive<'a> {
    socket: &'a mut credentials::Stream,
    checkpoint: &'a mut dyn FnMut() -> io::Result<()>,
    end: Instant,
    first: Instant,
    transfer: Option<Instant>,
    idle: Instant,
}
impl Read for Receive<'_> {
    fn read(&mut self, bytes: &mut [u8]) -> io::Result<usize> {
        loop {
            (self.checkpoint)()?;
            let now = Instant::now();
            let deadline = self
                .end
                .min(self.transfer.map_or(self.first, |t| t.min(self.idle)));
            if now >= deadline {
                return Err(io::Error::new(
                    io::ErrorKind::TimedOut,
                    "control response deadline",
                ));
            }
            self.socket
                .set_read_timeout(Some((deadline - now).min(Duration::from_millis(100))))?;
            match self.socket.read(bytes) {
                Ok(n) => {
                    (self.checkpoint)()?;
                    if Instant::now() >= deadline {
                        return Err(io::Error::new(
                            io::ErrorKind::TimedOut,
                            "control response deadline",
                        ));
                    }
                    if n > 0 {
                        let now = Instant::now();
                        self.transfer.get_or_insert(now + Duration::from_secs(10));
                        self.idle = now + Duration::from_secs(2);
                    }
                    return Ok(n);
                }
                Err(e)
                    if matches!(
                        e.kind(),
                        io::ErrorKind::WouldBlock
                            | io::ErrorKind::TimedOut
                            | io::ErrorKind::Interrupted
                    ) => {}
                Err(e) => return Err(e),
            }
        }
    }
}

// Exactly one non-pipelined connection, bound to one endpoint and credential
// provider. Taking the idle socket makes every error discard it. The subscriber's
// bounded backoff retries with freshly generated observation headers.
struct ControlTransport {
    address: SocketAddr,
    host: String,
    target: String,
    provider: Arc<credentials::Provider>,
    idle: Option<credentials::Stream>,
    lifetime: Duration,
}
impl ControlTransport {
    #[cfg(test)]
    fn fetch(
        &mut self,
        etag: Option<&str>,
        headers: &[(&str, String)],
        checkpoint: &mut dyn FnMut() -> io::Result<()>,
    ) -> io::Result<(Option<Vec<u8>>, Option<String>)> {
        self.fetch_with_first_byte_timeout(etag, headers, checkpoint, CONTROL_FIRST_BYTE_TIMEOUT)
    }
    fn fetch_with_first_byte_timeout(
        &mut self,
        etag: Option<&str>,
        headers: &[(&str, String)],
        checkpoint: &mut dyn FnMut() -> io::Result<()>,
        first_byte_timeout: Duration,
    ) -> io::Result<(Option<Vec<u8>>, Option<String>)> {
        let idle = self
            .idle
            .take()
            .filter(|socket| socket.reusable(&self.provider));
        checkpoint()?;
        let start = Instant::now();
        let host = &self.host;
        let target = &self.target;
        http::Request::new(target, &[])?;
        if host.is_empty() || !host.bytes().all(|b| b.is_ascii_graphic()) {
            return Err(invalid("invalid control host"));
        }
        let mut request = format!(
            "GET {target} HTTP/1.1\r\nHost: {host}\r\nAccept: application/x-protobuf\r\nPrefer: wait=28\r\nContent-Length: 0\r\n"
        );
        if let Some(etag) = etag {
            http::Request::new("/", &[("If-None-Match", etag)])?;
            request.push_str(&format!("If-None-Match: {etag}\r\n"));
        }
        for (name, value) in headers {
            http::Request::new("/", &[(name, value)])?;
            request.push_str(&format!("{name}: {value}\r\n"));
        }
        let mut socket = match idle {
            Some(socket) => socket,
            None => self
                .provider
                .connect_with_lifetime(self.address, self.lifetime)?,
        };
        // Generate trust claims only after retiring an old idle connection. Check
        // the revision after reading the claims so new trust is never claimed over
        // an old authenticated context. In-flight requests may finish within their
        // existing deadline; they remain counted until their socket is dropped.
        for (name, value) in self.provider.headers() {
            http::Request::new("/", &[(name, &value)])?;
            request.push_str(&format!("{name}: {value}\r\n"));
        }
        if !socket.reusable(&self.provider) {
            return Err(invalid("control credentials changed or expire soon"));
        }
        request.push_str("\r\n");
        socket.set_write_timeout(Some(Duration::from_millis(100)))?;
        // Check cancellation/deadline even if an adversarial peer accepts tiny writes.
        let mut remaining = request.as_bytes();
        while !remaining.is_empty() {
            checkpoint()?;
            if start.elapsed() >= Duration::from_secs(2) {
                return Err(invalid("control send deadline"));
            }
            let n = socket.write(remaining)?;
            if n == 0 {
                return Err(io::ErrorKind::WriteZero.into());
            }
            remaining = &remaining[n..];
        }
        // The server holds unchanged requests for 27-30 seconds. First-byte
        // waiting is separate from the bounded ten-second response transfer.
        let start = Instant::now();
        let end = start + Duration::from_secs(45);
        let mut reader = std::io::BufReader::new(Receive {
            socket: &mut socket,
            checkpoint,
            end,
            first: start + first_byte_timeout,
            transfer: None,
            idle: start,
        });
        let response = read_framed_response(&mut reader, etag, LIMIT)?;
        // No pipelining: bytes beyond this frame cannot belong to another response.
        if !reader.buffer().is_empty() {
            return Err(invalid("unsolicited bytes after control response"));
        }
        if response.reusable && socket.reusable(&self.provider) {
            self.idle = Some(socket);
        }
        Ok((response.body, response.etag))
    }
}

fn read_response(
    reader: &mut impl BufRead,
    etag: Option<&str>,
    limit: usize,
) -> io::Result<(Option<Vec<u8>>, Option<String>)> {
    let response = read_framed_response(reader, etag, limit)?;
    Ok((response.body, response.etag))
}

struct ControlResponse {
    body: Option<Vec<u8>>,
    etag: Option<String>,
    reusable: bool,
}

fn read_framed_response(
    reader: &mut impl BufRead,
    etag: Option<&str>,
    limit: usize,
) -> io::Result<ControlResponse> {
    let mut header = Vec::new();
    loop {
        if header.len() >= 8192 {
            return Err(invalid("control header budget exceeded"));
        }
        let mut byte = [0];
        reader.read_exact(&mut byte)?;
        header.push(byte[0]);
        if header.ends_with(b"\r\n\r\n") {
            break;
        }
    }
    let text = std::str::from_utf8(&header).map_err(invalid)?;
    let mut lines = text.split("\r\n");
    let mut status_line = lines.next().unwrap().splitn(3, ' ');
    let version = status_line.next().unwrap();
    if !matches!(version, "HTTP/1.1" | "HTTP/1.0") {
        return Err(invalid("invalid control HTTP version"));
    }
    let status = status_line
        .next()
        .ok_or_else(|| invalid("missing HTTP status"))?;
    let mut reusable = version == "HTTP/1.1";
    let mut length = None;
    let mut next_etag = None;
    for line in lines.filter(|l| !l.is_empty()) {
        let (name, value) = line
            .split_once(':')
            .ok_or_else(|| invalid("invalid control header"))?;
        let value = value.trim();
        if name.is_empty()
            || !name.bytes().all(crate::http::token)
            || !crate::http::value(value.as_bytes())
        {
            return Err(invalid("invalid control header"));
        }
        if name.eq_ignore_ascii_case("connection")
            && value
                .split(',')
                .any(|token| token.trim().eq_ignore_ascii_case("close"))
        {
            reusable = false;
        }
        if name.eq_ignore_ascii_case("transfer-encoding") {
            return Err(invalid("control transfer encoding unsupported"));
        }
        if name.eq_ignore_ascii_case("content-length") {
            if value.is_empty() || !value.bytes().all(|b| b.is_ascii_digit()) {
                return Err(invalid("invalid control content length"));
            }
            let size: usize = value.parse().map_err(invalid)?;
            if size > limit || length.replace(size).is_some() {
                return Err(invalid("invalid control content length"));
            }
        }
        if name.eq_ignore_ascii_case("etag") && next_etag.replace(value.to_owned()).is_some() {
            return Err(invalid("duplicate etag"));
        }
    }
    if status == "304" && etag.is_some() {
        // A 304 has no body; its optional length describes the selected representation.
        return Ok(ControlResponse {
            body: None,
            etag: None,
            reusable,
        });
    }
    if status == "204" && length.is_none_or(|length| length == 0) {
        return Ok(ControlResponse {
            body: None,
            etag: None,
            reusable,
        });
    }
    if status != "200" {
        return Err(invalid("unexpected control response"));
    }
    let length = length.ok_or_else(|| invalid("missing control content length"))?;
    let mut body = Vec::with_capacity(length);
    while body.len() < length {
        let available = reader.fill_buf()?;
        if available.is_empty() {
            return Err(invalid("truncated control body"));
        }
        let n = available.len().min(length - body.len());
        body.extend_from_slice(&available[..n]);
        reader.consume(n);
    }
    Ok(ControlResponse {
        body: Some(body),
        etag: next_etag,
        reusable,
    })
}

struct Watch {
    file: uring::File,
    path: PathBuf,
    ticket: Option<uring::Ticket<uring::Control>>,
    watches: BTreeSet<i32>,
}
impl Watch {
    fn new(path: PathBuf) -> io::Result<Self> {
        // SAFETY: returns a uniquely owned nonblocking descriptor.
        let fd = unsafe { libc::inotify_init1(libc::IN_NONBLOCK | libc::IN_CLOEXEC) };
        if fd < 0 {
            return Err(io::Error::last_os_error());
        }
        let file = uring::File::new(unsafe { OwnedFd::from_raw_fd(fd) });
        let mut watch = Self {
            file,
            path,
            ticket: None,
            watches: BTreeSet::new(),
        };
        watch.arm_paths()?;
        Ok(watch)
    }
    fn arm_paths(&mut self) -> io::Result<()> {
        // Parent events cover atomic replacement and Kubernetes ..data swaps.
        let parent = self
            .path
            .parent()
            .filter(|p| !p.as_os_str().is_empty())
            .unwrap_or(Path::new("."));
        let mut watches = BTreeSet::from([self.add(parent)?]);
        if self.path.exists() {
            watches.insert(self.add(&self.path)?);
        }
        if let Ok(target) = self.path.canonicalize() {
            if let Some(parent) = target.parent() {
                watches.insert(self.add(parent)?);
            }
        }
        for old in self.watches.difference(&watches) {
            // SAFETY: a live inotify fd; already-removed watches return EINVAL.
            unsafe {
                libc::inotify_rm_watch(self.file.as_fd().as_raw_fd(), *old);
            }
        }
        self.watches = watches;
        Ok(())
    }
    fn add(&self, path: &Path) -> io::Result<i32> {
        let name = CString::new(path.as_os_str().as_bytes()).map_err(invalid)?;
        let mask = libc::IN_CLOSE_WRITE
            | libc::IN_MOVED_TO
            | libc::IN_CREATE
            | libc::IN_DELETE
            | libc::IN_DELETE_SELF
            | libc::IN_MOVE_SELF
            | libc::IN_ATTRIB;
        // SAFETY: NUL-terminated path and live inotify fd.
        let watch =
            unsafe { libc::inotify_add_watch(self.file.as_fd().as_raw_fd(), name.as_ptr(), mask) };
        if watch < 0 {
            return Err(io::Error::last_os_error());
        }
        Ok(watch)
    }
    fn changed(&mut self, ring: &mut uring::Ring) -> io::Result<bool> {
        if let Some(ticket) = &mut self.ticket {
            if ring.take_control(ticket)?.is_none() {
                return Ok(false);
            }
            self.ticket = None;
        }
        let mut changed = false;
        let mut bytes = [0u8; 8192];
        // Bounded drain; all events, including overflow, mean rescan the file.
        for _ in 0..16 {
            let n = unsafe {
                libc::read(
                    self.file.as_fd().as_raw_fd(),
                    bytes.as_mut_ptr().cast(),
                    bytes.len(),
                )
            };
            if n > 0 {
                changed = true;
                continue;
            }
            if n < 0 && io::Error::last_os_error().kind() != io::ErrorKind::WouldBlock {
                return Err(io::Error::last_os_error());
            }
            break;
        }
        if changed {
            self.arm_paths()?;
        }
        match ring.poll_fd(self.file.clone().into(), uring::Readiness::Readable) {
            Ok(t) => self.ticket = Some(t.cancel_on_drop()),
            Err(e) if e.kind() == io::ErrorKind::WouldBlock => {}
            Err(e) => return Err(e),
        }
        Ok(changed)
    }
    fn load(&self) -> io::Result<proto::Configuration> {
        let mut bytes = Vec::new();
        std::fs::File::open(&self.path)?
            .take((LIMIT + 1) as u64)
            .read_to_end(&mut bytes)?;
        if bytes.len() > LIMIT {
            return Err(invalid("configuration exceeds 4 MiB"));
        }
        serde_json::from_slice(&bytes).map_err(invalid)
    }
}

pub struct Client {
    watch: Option<Watch>,
    trust: Arc<Trust>,
    updates: Arc<Updates>,
    next: Instant,
    failures: u32,
    dirty: bool,
}

#[cfg(test)]
#[path = "../tests/control/configuration.rs"]
pub(crate) mod tests;
impl Client {
    pub fn new(source: Source, trust: Arc<Trust>, updates: Arc<Updates>) -> io::Result<Self> {
        let watch = match &source {
            Source::File(path) => Some(Watch::new(path.clone())?),
            _ => return Err(invalid("use Subscriber for coordinated HTTP control")),
        };
        Ok(Self {
            watch,
            trust,
            updates,
            next: crate::environment::now(),
            failures: 0,
            dirty: true,
        })
    }
    fn accept(&self, config: proto::Configuration) -> io::Result<()> {
        self.updates.publish(self.trust.prepare(config)?)
    }
    pub fn poll(&mut self, ring: &mut uring::Ring, budget: usize) -> uring::Work {
        match self.poll_inner(ring, budget) {
            Ok(work) => work,
            Err(error) => {
                eprintln!("control-plane update rejected: {error}");
                self.failures = (self.failures + 1).min(6);
                let mut jitter = [0; 2];
                let _ = crate::environment::random(&mut jitter);
                self.next = crate::environment::now()
                    + Duration::from_millis(
                        (250 << self.failures) + u16::from_le_bytes(jitter) as u64 % 250,
                    );
                uring::Work {
                    runnable: false,
                    deadline: Some(self.next),
                }
            }
        }
    }
    fn poll_inner(&mut self, ring: &mut uring::Ring, _budget: usize) -> io::Result<uring::Work> {
        let now = crate::environment::now();
        if let Some(watch) = &mut self.watch {
            if watch.changed(ring)? {
                self.dirty = true;
                self.next = now;
            }
            if self.dirty && now >= self.next {
                let config = watch.load()?;
                self.accept(config)?;
                self.dirty = false;
                self.failures = 0;
            }
            return Ok(uring::Work {
                runnable: false,
                deadline: if self.dirty {
                    Some(self.next)
                } else if self.watch.as_ref().unwrap().ticket.is_none() {
                    Some(now + Duration::from_millis(10))
                } else {
                    None
                },
            });
        }
        Ok(uring::Work::default())
    }
    pub fn shutdown(&mut self, _ring: &mut uring::Ring) -> io::Result<()> {
        self.watch = None;
        Ok(())
    }
}

pub mod routing {
    //! Immutable local topology and bounded, validated fault routing cursors.
    use crate::{
        control::proto,
        topology::{Epoch, Step, Topology},
    };
    use std::{
        collections::{BTreeMap, BTreeSet},
        io,
    };

    fn invalid() -> io::Error {
        io::Error::new(
            io::ErrorKind::InvalidData,
            "invalid topology routing context",
        )
    }

    #[derive(Clone, Copy, Debug, Eq, PartialEq, Hash)]
    pub enum Algorithm {
        Canonical = 1,
    }
    impl Algorithm {
        pub fn wire_version(self) -> u8 {
            match self {
                Self::Canonical => 1,
            }
        }
        pub fn magic(self) -> &'static [u8; 4] {
            match self {
                Self::Canonical => b"RR01",
            }
        }
    }

    pub struct Routing {
        pub algorithm: Algorithm,
        pub geometry: Topology,
        pub local: BTreeSet<u32>,
        pub neighbors: BTreeMap<u32, String>,
        pub identity: [u8; 32],
        #[cfg(test)]
        namespace: crate::cache::Namespace,
    }
    impl Routing {
        pub fn new(universe: &[u8], volume: &proto::Volume) -> io::Result<Self> {
            let config = volume.topology.clone().unwrap_or(proto::Topology {
                routing_algorithm: Some(1),
                epoch: 1,
                slot_count: 1,
                local_slots: vec![0],
                neighbors: vec![],
            });
            let algorithm = match config.routing_algorithm {
                Some(1) => Algorithm::Canonical,
                _ => return Err(invalid()),
            };
            if (volume.topology.is_none() && !volume.peers.is_empty())
                || config.epoch == 0
                || config.slot_count > super::MAX_SLOTS
                || volume.peers.len() > 100000
                || config.local_slots.len() > super::MAX_SLOTS as usize
                || config.neighbors.len() > super::MAX_SLOTS as usize
            {
                return Err(invalid());
            }
            let geometry = Topology::new(config.slot_count, Epoch::new(config.epoch))
                .map_err(|_| invalid())?;
            let local: BTreeSet<_> = config.local_slots.iter().copied().collect();
            if local.is_empty()
                || local.len() != config.local_slots.len()
                || local.iter().any(|s| geometry.slot(*s).is_err())
            {
                return Err(invalid());
            }
            let mut required = vec![false; config.slot_count as usize];
            let mut local_bits = vec![false; config.slot_count as usize];
            for &s in &local {
                local_bits[s as usize] = true;
            }
            for &s in &local {
                for digit in 0..geometry.degree() {
                    let next = ((u64::from(s) * u64::from(geometry.degree()) + u64::from(digit))
                        % u64::from(config.slot_count)) as u32;
                    if !local_bits[next as usize] {
                        required[next as usize] = true;
                    }
                }
            }
            let mut neighbors = BTreeMap::new();
            let peers: BTreeSet<_> = volume.peers.iter().collect();
            let mut used = BTreeSet::new();
            for n in &config.neighbors {
                if !required.get(n.slot as usize).copied().unwrap_or(false)
                    || !peers.contains(&n.peer)
                    || neighbors.insert(n.slot, n.peer.clone()).is_some()
                {
                    return Err(invalid());
                }
                used.insert(&n.peer);
            }
            if neighbors.len() != required.iter().filter(|b| **b).count() || peers != used {
                return Err(invalid());
            }
            let mut identity = blake3::Hasher::new();
            identity.update(b"racer/topology/v1");
            identity.update(universe);
            identity.update(volume.id.as_bytes());
            identity.update(&volume.cache_generation.to_le_bytes());
            identity.update(&config.epoch.to_le_bytes());
            identity.update(&config.slot_count.to_le_bytes());
            identity.update(b"/algorithm/");
            identity.update(&(algorithm as u32).to_le_bytes());
            Ok(Self {
                #[cfg(test)]
                namespace: crate::cache::Namespace::volume(
                    universe,
                    &volume.id,
                    volume.cache_generation,
                    crate::cache::Namespace::new(&volume.id)
                        .map_err(crate::cache::Error::into_io)?,
                ),
                algorithm,
                geometry,
                local,
                neighbors,
                identity: *identity.finalize().as_bytes(),
            })
        }
        #[cfg(test)]
        pub(crate) fn start(&self, target: &str) -> Cursor {
            self.start_key(
                &crate::cache::PeerDescriptor::metadata(target)
                    .key(self.namespace)
                    .unwrap(),
            )
        }
        pub fn start_key(&self, key: &[u8; 32]) -> Cursor {
            let owner = self.geometry.owner(key).get();
            Cursor {
                algorithm: self.algorithm,
                identity: self.identity,
                source: *self.local.first().unwrap(),
                owner,
                attempt: 0,
                position: 0,
            }
        }
        pub fn destination(&self, c: &Cursor) -> u32 {
            ((u64::from(c.owner) + u64::from(c.attempt)) % u64::from(self.geometry.slot_count()))
                as u32
        }
        /// Namespace authentication is performed by the caller before rebasing.
        /// Placement revisions are hints, not immutable object identity.
        pub fn receive(&self, c: Cursor, key: &[u8; 32]) -> io::Result<Cursor> {
            if c.attempt >= 8 {
                return Err(invalid());
            }
            if c.identity != self.identity {
                let mut local = self.start_key(key);
                // Candidate retries remain bounded independently of placement.
                local.attempt = c.attempt.min(self.geometry.slot_count() - 1);
                return Ok(local);
            }
            self.validate(&c, key)?;
            Ok(c)
        }
        fn path(&self, c: &Cursor) -> io::Result<Vec<u32>> {
            if c.identity != self.identity
                || c.algorithm != self.algorithm
                || c.owner >= self.geometry.slot_count()
                || c.attempt >= self.geometry.slot_count()
            {
                return Err(invalid());
            }
            let mut route = self
                .geometry
                .route(
                    self.geometry.slot(c.source).map_err(|_| invalid())?,
                    self.geometry
                        .slot(self.destination(c))
                        .map_err(|_| invalid())?,
                )
                .map_err(|_| invalid())?;
            let mut path = vec![c.source];
            while let Step::Forward { next } = route.advance() {
                if let Some(index) = path.iter().position(|s| *s == next.get()) {
                    path.truncate(index + 1);
                } else {
                    path.push(next.get());
                }
            }
            Ok(path)
        }
        pub fn validate(&self, c: &Cursor, key: &[u8; 32]) -> io::Result<()> {
            if c.owner != self.geometry.owner(key).get() {
                return Err(invalid());
            }
            let path = self.path(c)?;
            if !path
                .get(c.position as usize)
                .is_some_and(|s| self.local.contains(s))
            {
                return Err(invalid());
            }
            Ok(())
        }
        pub fn next(&self, c: &Cursor) -> io::Result<Option<(String, Cursor)>> {
            let path = self.path(c)?;
            let mut next = c.clone();
            // Co-located later slots are local transitions; bypassing the intervening
            // network segment prevents a shared cache flight from waiting on itself.
            next.position = self.normalized_position(c)?;
            for &slot in path.iter().skip(next.position as usize + 1) {
                next.position += 1;
                if !self.local.contains(&slot) {
                    return Ok(Some((
                        self.neighbors.get(&slot).ok_or_else(invalid)?.clone(),
                        next,
                    )));
                }
            }
            Ok(None)
        }
        pub fn normalized_position(&self, c: &Cursor) -> io::Result<u8> {
            let path = self.path(c)?;
            if !path
                .get(c.position as usize)
                .is_some_and(|s| self.local.contains(s))
            {
                return Err(invalid());
            }
            path.iter()
                .enumerate()
                .skip(c.position as usize)
                .filter(|(_, slot)| self.local.contains(slot))
                .map(|(i, _)| i as u8)
                .next_back()
                .ok_or_else(invalid)
        }
        /// Local shortcuts only move down the canonical rank. Different local
        /// slots need distinct flights: physical-host identity can create cycles.
        pub fn dependency(&self, c: &Cursor) -> io::Result<crate::buffers::NetworkDependency> {
            let position = self.normalized_position(c)?;
            Ok(crate::buffers::NetworkDependency::Canonical {
                slot: self.path(c)?[position as usize],
            })
        }
        pub fn compatible(&self, a: &Cursor, b: &Cursor) -> bool {
            a.identity == b.identity
                && self.destination(a) == self.destination(b)
                && self
                    .dependency(a)
                    .ok()
                    .zip(self.dependency(b).ok())
                    .is_some_and(|(a, b)| a == b)
        }
        /// Sparse slot ownership proves physical finality, never endpoint equality.
        pub fn last_hop(&self, c: &Cursor) -> bool {
            self.final_peer(c).is_some()
        }
        pub(crate) fn final_peer(&self, c: &Cursor) -> Option<&str> {
            if c.identity != self.identity {
                return None;
            }
            let (peer, _) = self.next(c).ok().flatten()?;
            self.neighbors
                .get(&self.destination(c))
                .filter(|owner| **owner == peer)
                .map(String::as_str)
        }
    }

    #[derive(Clone, Debug)]
    pub struct Cursor {
        pub algorithm: Algorithm,
        pub identity: [u8; 32],
        pub source: u32,
        pub owner: u32,
        pub attempt: u32,
        pub position: u8,
    }
    impl Cursor {
        pub const LEN: usize = 45;
        /// Encode the cursor body. The enclosing RR01 magic must
        /// be selected from `algorithm`; the body alone is not a wire descriptor.
        pub fn encode(&self) -> Vec<u8> {
            let mut bytes = self.identity.to_vec();
            bytes.extend(self.source.to_le_bytes());
            bytes.extend(self.owner.to_le_bytes());
            bytes.extend(self.attempt.to_le_bytes());
            bytes.push(self.position);
            bytes
        }
        /// Decode a canonical RR01 body.
        pub fn decode(bytes: &[u8]) -> io::Result<Self> {
            Self::decode_algorithm(bytes, Algorithm::Canonical)
        }
        pub fn decode_algorithm(bytes: &[u8], algorithm: Algorithm) -> io::Result<Self> {
            if bytes.len() != Self::LEN || bytes[44] > 3 {
                return Err(invalid());
            }
            Ok(Self {
                algorithm,
                identity: bytes[..32].try_into().unwrap(),
                source: u32::from_le_bytes(bytes[32..36].try_into().unwrap()),
                owner: u32::from_le_bytes(bytes[36..40].try_into().unwrap()),
                attempt: u32::from_le_bytes(bytes[40..44].try_into().unwrap()),
                position: bytes[44],
            })
        }
    }

    #[cfg(test)]
    include!(concat!(
        env!("CARGO_MANIFEST_DIR"),
        "/tests/control/routing.rs"
    ));
}
