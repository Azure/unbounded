// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

use super::*;
use anyhow::{Context, bail};
use std::{
    collections::{BTreeMap, BTreeSet},
    future::Future,
    sync::{
        Arc,
        atomic::{AtomicBool, Ordering},
    },
};

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum Phase {
    Stable,
    Overlap,
    Switched,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct LeafRecord {
    pub root: String,
    pub expiry: i64,
}

#[derive(Clone, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct ProofRecord {
    bundle: String,
    root: String,
    fence: String,
    at: i64,
    session: String,
    drained: bool,
}

#[derive(Clone, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Participant {
    pub identity: Identity,
    pub leaves: BTreeMap<String, LeafRecord>,
    #[serde(skip)]
    proof: Option<ProofRecord>,
}

#[derive(Clone, Default, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct Participants {
    members: BTreeMap<String, Participant>,
    retired: BTreeSet<String>,
}

/// Complete validated state in memory. Encoding separates participant shards
/// from Secret metadata. Never serialize this type into a single Kubernetes object.
#[derive(Clone)]
pub struct CaState {
    metadata: Metadata,
    participants: Participants,
}

#[derive(Clone, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct Metadata {
    version: u32,
    fence: String,
    fence_at: i64,
    generation: u64,
    active: String,
    phase: Phase,
    authorities: Vec<Authority>,
    shards: BTreeMap<String, String>,
    #[serde(default)]
    rotation_nonce: String,
}

/// Immutable content-addressed participant blobs are written before the Secret
/// CAS. `metadata` alone is the commit point. A failed CAS may leave unreachable
/// shards; collection must never remove shards reachable by a current reader.
/// No private material is included in the shards.
#[derive(Clone, PartialEq, Eq)]
pub struct StateImage {
    pub metadata: Vec<u8>,
    pub shards: BTreeMap<String, Vec<u8>>,
}

fn bucket(key: &str) -> String {
    let hash = Sha256::digest(key.as_bytes());
    format!("{:03x}", u16::from_be_bytes([hash[0], hash[1]]) & 1023)
}

fn encode<T: Serialize>(value: &T) -> Result<Vec<u8>> {
    let bytes = serde_json::to_vec(value)?;
    ensure!(
        bytes.len() <= MAX_OBJECT_BYTES,
        "PKI object capacity exhausted"
    );
    Ok(bytes)
}

impl CaState {
    pub fn bundle(&self) -> TrustBundle {
        TrustBundle {
            version: 1,
            generation: self.metadata.generation,
            active: self.metadata.active.clone(),
            certificates: self
                .metadata
                .authorities
                .iter()
                .map(|a| a.certificate.as_str())
                .collect(),
        }
    }

    pub fn phase(&self) -> Phase {
        self.metadata.phase
    }
    pub fn fence(&self) -> &str {
        &self.metadata.fence
    }
    pub fn members(&self) -> impl Iterator<Item = &Participant> {
        self.participants.members.values()
    }
    pub fn member(&self, key: &str) -> Option<&Participant> {
        self.participants.members.get(key)
    }
    pub fn expiry_watermarks(&self) -> impl Iterator<Item = (&str, i64)> {
        self.metadata
            .authorities
            .iter()
            .map(|a| (a.digest.as_str(), a.last_issued_expiry))
    }

    pub fn proof_root(&self) -> &str {
        if self.phase() == Phase::Overlap {
            &self.metadata.authorities[1].digest
        } else {
            &self.metadata.active
        }
    }

    pub fn to_image(&self) -> Result<StateImage> {
        let mut buckets: BTreeMap<String, Participants> = BTreeMap::new();
        for (key, member) in &self.participants.members {
            buckets
                .entry(bucket(key))
                .or_default()
                .members
                .insert(key.clone(), member.clone());
        }
        for key in &self.participants.retired {
            buckets
                .entry(bucket(key))
                .or_default()
                .retired
                .insert(key.clone());
        }
        let mut metadata = self.metadata.clone();
        metadata.shards.clear();
        let mut shards = BTreeMap::new();
        for (bucket, participants) in buckets {
            let bytes = encode(&participants)?;
            let id = digest(&bytes);
            metadata.shards.insert(bucket, id.clone());
            shards.insert(id, bytes);
        }
        Ok(StateImage {
            metadata: encode(&metadata)?,
            shards,
        })
    }

    pub fn from_image(image: &StateImage) -> Result<Self> {
        ensure!(
            image.metadata.len() <= MAX_OBJECT_BYTES,
            "CA metadata too large"
        );
        let metadata: Metadata = serde_json::from_slice(&image.metadata)?;
        ensure!(
            metadata.version == 4
                && !metadata.fence.is_empty()
                && metadata.fence_at > 0
                && metadata.generation > 0,
            "invalid CA state metadata"
        );
        ensure!(
            metadata.authorities.len()
                == if metadata.phase == Phase::Stable {
                    1
                } else {
                    2
                },
            "invalid CA phase"
        );
        let active = if metadata.phase == Phase::Switched {
            1
        } else {
            0
        };
        ensure!(
            metadata.active == metadata.authorities[active].digest,
            "invalid active CA"
        );
        for authority in &metadata.authorities {
            authority.parse()?;
        }
        let mut participants = Participants::default();
        ensure!(
            metadata.shards.len() <= 1024,
            "too many participant buckets"
        );
        for (bucket_id, id) in &metadata.shards {
            ensure!(hex_id(id), "invalid shard reference");
            let bytes = image
                .shards
                .get(id)
                .context("missing committed participant shard")?;
            ensure!(
                bytes.len() <= MAX_OBJECT_BYTES && digest(bytes) == *id,
                "participant shard integrity failure"
            );
            let shard: Participants = serde_json::from_slice(bytes)?;
            for (key, member) in shard.members {
                ensure!(
                    bucket(&key) == *bucket_id
                        && member.identity.key() == key
                        && !shard.retired.contains(&key),
                    "invalid participant"
                );
                member.identity.uri()?;
                for (fp, leaf) in &member.leaves {
                    ensure!(
                        hex_id(fp) && hex_id(&leaf.root) && leaf.expiry > 0,
                        "invalid leaf record"
                    );
                    if let Some(authority) =
                        metadata.authorities.iter().find(|a| a.digest == leaf.root)
                    {
                        ensure!(
                            leaf.expiry <= authority.last_issued_expiry,
                            "leaf exceeds issuer watermark"
                        );
                    }
                }
                participants.members.insert(key, member);
            }
            for key in shard.retired {
                let (uid, boot) = key.split_once('/').context("invalid retirement key")?;
                ensure!(
                    bucket(&key) == *bucket_id && process_id(uid) && process_id(boot),
                    "invalid tombstone"
                );
                participants.retired.insert(key);
            }
        }
        let state = Self {
            metadata,
            participants,
        };
        TrustBundle::parse(&state.bundle().json())?;
        Ok(state)
    }

    fn admit(&mut self, identity: Identity) -> Result<&mut Participant> {
        identity.uri()?;
        ensure!(
            identity.kind != IdentityKind::Node
                || self.members().all(|member| {
                    member.identity.pod_uid != identity.pod_uid
                        || (member.identity.kind == IdentityKind::Node
                            && member.identity.universe == identity.universe
                            && member.identity.node == identity.node)
                }),
            "Pod cannot enroll in a different Node or universe before replacement"
        );
        let key = identity.key();
        ensure!(
            !self.participants.retired.contains(&key),
            "process was authoritatively retired"
        );
        let member = self
            .participants
            .members
            .entry(key)
            .or_insert_with(|| Participant {
                identity: identity.clone(),
                leaves: BTreeMap::new(),
                proof: None,
            });
        if member.identity.container_id.is_empty() {
            member
                .identity
                .container_id
                .clone_from(&identity.container_id);
        }
        if member.identity.pod_name.is_empty() {
            member.identity.pod_name.clone_from(&identity.pod_name);
        }
        ensure!(
            member.identity == identity,
            "durable process identity cannot change"
        );
        Ok(member)
    }

    fn next_generation(&mut self) -> Result<()> {
        self.metadata.generation = self
            .metadata
            .generation
            .checked_add(1)
            .context("trust generation exhausted")?;
        Ok(())
    }

    /// Requires transport-created identity plus exact durable leaf/boot binding.
    pub fn verify_member(&self, key: &str, peer: &VerifiedPeer, now: i64) -> Result<&Participant> {
        let member = self.member(key).context("unknown durable process")?;
        let leaf = member
            .leaves
            .get(&peer.fingerprint)
            .context("TLS leaf not enrolled for boot")?;
        ensure!(
            now >= peer.not_before && now < peer.not_after && now < leaf.expiry,
            "expired TLS identity"
        );
        ensure!(
            leaf.root == peer.root && member.identity.uri()? == peer.uri,
            "TLS identity mismatch"
        );
        Ok(member)
    }
}

#[derive(Clone)]
pub struct Publication {
    pub resource_version: String,
    pub fence: String,
    pub bytes: Vec<u8>,
}

#[derive(Clone, Default)]
pub struct StoreSnapshot {
    pub resource_version: Option<String>,
    pub image: Option<StateImage>,
    pub publication: Option<Publication>,
    /// An authoritative bootstrap sweep found trust/topology/replica artifacts.
    /// True forbids creating replacement private state even for an empty public tombstone.
    pub prior_artifacts: bool,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum CommitOutcome {
    Committed,
    Conflict,
    Uncertain,
}

/// Async Kubernetes seam. All reads are authoritative, not informer-cache reads.
///
/// `commit` creates immutable shards, verifies existing content on AlreadyExists,
/// then CASes the Secret using `expected.resource_version`. It checks the old
/// persisted fence as well. Bootstrap is create-only and must reject existing
/// public/topology/replica artifacts. Never delete/recreate on an uncertain write.
///
/// `publish` must fence the public object too: capture its resourceVersion before
/// rereading the Secret fence/revision, then CAS the public object. A takeover
/// claims the public object's fence using this same ordering before serving.
/// A stale writer may never retry with a freshly fetched public version. Both
/// methods return Uncertain for timeouts whose durable outcome is unknown.
pub trait CaStore: Send + Sync {
    fn collect(&self) -> impl Future<Output = Result<()>> + Send {
        async { Ok(()) }
    }
    fn read(&self) -> impl Future<Output = Result<StoreSnapshot>> + Send;
    fn commit(
        &self,
        expected: &StoreSnapshot,
        next: &StateImage,
    ) -> impl Future<Output = Result<CommitOutcome>> + Send;
    fn publish(
        &self,
        expected: &StoreSnapshot,
        fence: &str,
        bundle: &[u8],
    ) -> impl Future<Output = Result<CommitOutcome>> + Send;
}

#[derive(Clone, Debug)]
pub struct SecurityOptions {
    pub namespace: String,
    pub leaf_lifetime: i64,
    pub ca_lifetime: i64,
    pub clock_skew: i64,
    pub proof_lifetime: i64,
}

impl SecurityOptions {
    pub fn new(namespace: impl Into<String>) -> Self {
        Self {
            namespace: namespace.into(),
            leaf_lifetime: 86400,
            ca_lifetime: 365 * 86400,
            clock_skew: 300,
            proof_lifetime: 300,
        }
    }
    fn validate(&self) -> Result<()> {
        ensure!(
            super::certificates::valid_namespace(&self.namespace),
            "invalid namespace"
        );
        ensure!(
            self.leaf_lifetime > 0 && self.clock_skew >= 0 && self.proof_lifetime > 0,
            "invalid PKI lifetimes"
        );
        let minimum = self
            .leaf_lifetime
            .checked_add(
                self.clock_skew
                    .checked_mul(2)
                    .context("lifetime overflow")?,
            )
            .context("lifetime overflow")?;
        ensure!(self.ca_lifetime > minimum, "CA lifetime too short");
        Ok(())
    }
}

/// Call cancel immediately on election loss. A unique random token must be used
/// for every leadership lifetime. Secret CAS fences paused predecessor managers.
#[derive(Clone)]
pub struct Leadership {
    pub(super) token: String,
    alive: Arc<AtomicBool>,
}

impl Leadership {
    pub fn token(&self) -> &str {
        &self.token
    }
    pub fn is_active(&self) -> bool {
        self.alive.load(Ordering::Acquire)
    }
    pub fn new(token: String) -> Result<Self> {
        ensure!(!token.is_empty(), "empty leadership fence");
        Ok(Self {
            token,
            alive: Arc::new(AtomicBool::new(true)),
        })
    }
    pub fn cancel(&self) {
        self.alive.store(false, Ordering::Release);
    }
    pub(super) fn check(&self) -> Result<()> {
        ensure!(self.alive.load(Ordering::Acquire), "leadership canceled");
        Ok(())
    }
}

pub struct CaManager<S> {
    store: S,
    term: Leadership,
    options: SecurityOptions,
    // Serializes local commits; cross-process serialization is the Secret CAS.
    writer: tokio::sync::Mutex<()>,
    committed: std::sync::RwLock<Option<CaState>>,
    observations: std::sync::Mutex<BTreeMap<String, ProofRecord>>,
}

/// Candidate identities captured before an authoritative Pod observation starts.
/// An observation cannot retire admissions made after this snapshot. Keep this
/// opaque so adapters cannot build candidates from a later participant read.
pub struct RetirementCandidates {
    fence: String,
    identities: BTreeMap<String, Identity>,
}

#[derive(Clone, Debug)]
pub struct IssuedCertificate {
    pub certificate_pem: Vec<u8>,
    pub not_after: i64,
    pub root_digest: String,
    pub bundle: TrustBundle,
}

impl IssuedCertificate {
    pub fn chain_pem(&self) -> Result<Vec<u8>> {
        let mut chain = self.certificate_pem.clone();
        for root in super::certificates::parse_certificates(self.bundle.certificates.as_bytes())? {
            if digest(&root.to_der()?) == self.root_digest {
                chain.extend(root.to_pem()?);
                return Ok(chain);
            }
        }
        bail!("issuer absent from bundle")
    }
}

impl<S: CaStore> CaManager<S> {
    /// Serializes immutable-object garbage collection with local commits. Store
    /// adapters must use term-specific names so a successor never reuses a name
    /// selected by an old collector.
    pub async fn collect(&self) -> Result<()> {
        let _guard = self.writer.lock().await;
        self.term.check()?;
        self.store.collect().await
    }
    /// External election must already have granted this term. Acquires both
    /// durable fences and repairs interrupted publication before returning.
    pub async fn acquire(
        store: S,
        term: Leadership,
        options: SecurityOptions,
        now: i64,
    ) -> Result<Self> {
        options.validate()?;
        term.check()?;
        ensure!(now > 0, "invalid fence time");
        let manager = Self {
            store,
            term,
            options,
            writer: tokio::sync::Mutex::new(()),
            committed: std::sync::RwLock::new(None),
            observations: std::sync::Mutex::new(BTreeMap::new()),
        };
        let snapshot = manager.store.read().await?;
        let mut state = if let Some(image) = &snapshot.image {
            ensure!(
                snapshot.resource_version.is_some(),
                "missing Secret revision"
            );
            let mut state = CaState::from_image(image)?;
            ensure!(
                state.fence() != manager.term.token,
                "leadership token reused"
            );
            state.metadata.fence.clone_from(&manager.term.token);
            state.metadata.fence_at = now;
            // Persist invalidation, even if the clock did not advance.
            for member in state.participants.members.values_mut() {
                member.proof = None;
            }
            state
        } else {
            ensure!(
                snapshot.resource_version.is_none()
                    && snapshot.publication.is_none()
                    && !snapshot.prior_artifacts,
                "lost CA state; refusing regeneration"
            );
            let ca =
                Authority::generate(now, manager.options.ca_lifetime, manager.options.clock_skew)?;
            CaState {
                metadata: Metadata {
                    version: 4,
                    fence: manager.term.token.clone(),
                    fence_at: now,
                    generation: 1,
                    active: ca.digest.clone(),
                    phase: Phase::Stable,
                    authorities: vec![ca],
                    shards: BTreeMap::new(),
                    rotation_nonce: String::new(),
                },
                participants: Participants::default(),
            }
        };
        state.metadata.shards.clear();
        manager.persist(&snapshot, &state).await?;
        manager.publish().await?;
        Ok(manager)
    }

    pub fn leadership(&self) -> Leadership {
        self.term.clone()
    }

    async fn load(&self) -> Result<(StoreSnapshot, CaState)> {
        self.term.check()?;
        let snapshot = self.store.read().await?;
        let mut state = CaState::from_image(snapshot.image.as_ref().context("lost CA state")?)?;
        ensure!(state.fence() == self.term.token, "not the fenced leader");
        let observations = self.observations.lock().unwrap();
        for (key, member) in &mut state.participants.members {
            member.proof = observations
                .get(key)
                .filter(|p| p.fence == self.term.token)
                .cloned();
        }
        Ok((snapshot, state))
    }

    pub async fn state(&self) -> Result<CaState> {
        Ok(self.load().await?.1)
    }

    /// Indexed immutable admission snapshot for request-time authentication.
    /// It contains only this manager's confirmed commits; fresh proof observations
    /// are deliberately omitted from request authorization state.
    pub fn committed_state(&self) -> Result<CaState> {
        self.term.check()?;
        self.committed
            .read()
            .unwrap()
            .clone()
            .context("CA not loaded")
    }

    fn ready(&self, snapshot: &StoreSnapshot, state: &CaState) -> Result<()> {
        let publication = snapshot
            .publication
            .as_ref()
            .context("trust not published")?;
        ensure!(
            publication.fence == self.term.token && publication.bytes == state.bundle().json(),
            "trust publication is not current"
        );
        Ok(())
    }

    async fn persist(&self, snapshot: &StoreSnapshot, state: &CaState) -> Result<()> {
        self.term.check()?;
        let image = state.to_image()?;
        let outcome = self.store.commit(snapshot, &image).await?;
        ensure!(
            outcome != CommitOutcome::Conflict,
            "CA CAS conflict; reread and retry operation"
        );
        // Even an ambiguous successful write must be authoritatively observed.
        // Exact metadata commits immutable content hashes and all watermarks.
        let observed = self.store.read().await?;
        let actual = observed.image.as_ref().context("CA commit not durable")?;
        CaState::from_image(actual)?;
        ensure!(
            actual.metadata == image.metadata,
            "CA commit outcome unresolved or leadership lost"
        );
        self.term.check()?;
        *self.committed.write().unwrap() = Some(state.clone());
        self.observations
            .lock()
            .unwrap()
            .retain(|key, _| state.participants.members.contains_key(key));
        Ok(())
    }

    /// Repairs publication without advancing rotation. Safe on restart.
    pub async fn publish(&self) -> Result<()> {
        let _guard = self.writer.lock().await;
        let (snapshot, state) = self.load().await?;
        let bytes = state.bundle().json();
        if let Some(old) = &snapshot.publication {
            let bundle = TrustBundle::parse(&old.bytes)?;
            ensure!(
                bundle.generation <= state.bundle().generation,
                "trust rollback"
            );
            ensure!(
                bundle.generation != state.bundle().generation || old.bytes == bytes,
                "trust equivocation"
            );
        }
        self.term.check()?;
        ensure!(
            self.store
                .publish(&snapshot, &self.term.token, &bytes)
                .await?
                != CommitOutcome::Conflict,
            "trust CAS conflict"
        );
        let (observed, current) = self.load().await?;
        ensure!(
            current.bundle().json() == bytes,
            "publication raced state transition"
        );
        self.ready(&observed, &current)
    }

    /// Admit active, idle, draining, and takeover-capable processes before
    /// advancing a rotation. An unknown CP boot uses the blocking `pending` boot.
    pub async fn admit(&self, identity: Identity) -> Result<()> {
        let _guard = self.writer.lock().await;
        let (snapshot, mut state) = self.load().await?;
        state.admit(identity)?;
        self.persist(&snapshot, &state).await
    }

    /// Call only after credential, direct ownership, and committed membership
    /// authorization. `probe` selects the pending root during overlap.
    pub async fn issue(
        &self,
        csr: &[u8],
        identity: Identity,
        probe: bool,
        now: i64,
    ) -> Result<IssuedCertificate> {
        let key = validate_csr(csr)?;
        ensure!(
            identity.boot_id != "pending",
            "pending process cannot receive a certificate"
        );
        let _guard = self.writer.lock().await;
        let (snapshot, mut state) = self.load().await?;
        self.ready(&snapshot, &state)?;
        state.admit(identity.clone())?;
        let root = if probe {
            state.proof_root()
        } else {
            &state.metadata.active
        }
        .to_owned();
        let ca = state
            .metadata
            .authorities
            .iter_mut()
            .find(|a| a.digest == root)
            .context("issuer missing")?;
        let (pem, expiry) = super::certificates::sign_leaf(
            ca,
            &key,
            &identity,
            &self.options.namespace,
            now,
            self.options.leaf_lifetime,
            self.options.clock_skew,
        )?;
        ca.last_issued_expiry = ca.last_issued_expiry.max(expiry);
        let cert = openssl::x509::X509::from_pem(&pem)?;
        let member = state.participants.members.get_mut(&identity.key()).unwrap();
        member
            .leaves
            .retain(|_, leaf| now <= leaf.expiry.saturating_add(self.options.clock_skew));
        member.leaves.insert(
            digest(&cert.to_der()?),
            LeafRecord {
                root: root.clone(),
                expiry,
            },
        );
        self.persist(&snapshot, &state).await?;
        Ok(IssuedCertificate {
            certificate_pem: pem,
            not_after: expiry,
            root_digest: root,
            bundle: state.bundle(),
        })
    }

    pub async fn begin_rotation(&self, now: i64) -> Result<()> {
        self.begin_rotation_requested(now, "").await
    }

    pub async fn begin_rotation_requested(&self, now: i64, nonce: &str) -> Result<()> {
        {
            let _guard = self.writer.lock().await;
            let (snapshot, mut state) = self.load().await?;
            self.ready(&snapshot, &state)?;
            if state.phase() == Phase::Stable
                && (nonce.is_empty() || state.metadata.rotation_nonce != nonce)
            {
                if !nonce.is_empty() {
                    state.metadata.rotation_nonce = nonce.into();
                }
                state.metadata.authorities.push(Authority::generate(
                    now,
                    self.options.ca_lifetime,
                    self.options.clock_skew,
                )?);
                state.metadata.phase = Phase::Overlap;
                state.next_generation()?;
                self.persist(&snapshot, &state).await?;
            }
        }
        self.publish().await
    }

    /// Claims alone intentionally have no API that grants rotation credit.
    /// This capability can only be minted by this module's fresh TLS transport.
    pub async fn record_proof(&self, key: &str, proof: TlsProof, now: i64) -> Result<()> {
        let _guard = self.writer.lock().await;
        self.term.check()?;
        // Proofs are volatile observations. Only fenced durable transitions use
        // them, and every takeover starts empty. Avoid API writes at fleet proof
        // frequency; admissions and expiry watermarks remain durable.
        let committed = self.committed.read().unwrap();
        let state = committed.as_ref().context("CA not loaded")?;
        let member = state.verify_member(key, &proof.peer, now)?;
        ensure!(
            proof.fence == self.term.token,
            "proof belongs to a different leadership term"
        );
        let bundle = state.bundle();
        ensure!(
            proof.bundle == bundle.digest()
                && proof.ack.generation == bundle.generation
                && proof.ack.digest == bundle.digest(),
            "proof bundle mismatch"
        );
        ensure!(
            proof.at >= state.metadata.fence_at
                && proof.at <= now
                && now - proof.at <= self.options.proof_lifetime,
            "stale proof"
        );
        let mut observations = self.observations.lock().unwrap();
        if let Some(old) = observations.get(key) {
            ensure!(
                proof.at >= old.at && proof.session != old.session,
                "replayed proof"
            );
        }
        match member.identity.kind {
            IdentityKind::Node => {
                ensure!(
                    !proof.peer.is_server && proof.local_root == state.proof_root(),
                    "node proof requires pending server root"
                );
                ensure!(
                    state.phase() == Phase::Overlap || proof.peer.root == state.metadata.active,
                    "node still uses old issuer"
                );
            }
            IdentityKind::ControlPlane => ensure!(
                proof.peer.is_server && proof.peer.root == state.proof_root(),
                "CP proof requires pending server-authenticated leaf"
            ),
        }
        let record = ProofRecord {
            bundle: bundle.digest(),
            root: state.proof_root().into(),
            fence: self.term.token.clone(),
            at: proof.at,
            session: proof.session,
            drained: proof.ack.old_connections_drained,
        };
        observations.insert(key.to_owned(), record);
        Ok(())
    }

    /// Runtime must complete a direct, unfiltered participant admission sweep
    /// before each call. Missing/invalid inventory must block this operation.
    /// No heartbeat timeout, leaf expiry, or topology exclusion evicts a member.
    pub async fn advance_rotation(&self, now: i64) -> Result<Phase> {
        let phase;
        {
            let _guard = self.writer.lock().await;
            let (snapshot, mut state) = self.load().await?;
            self.ready(&snapshot, &state)?;
            let bundle = state.bundle().digest();
            let all_proven = state.participants.members.values().all(|p| {
                p.proof.as_ref().is_some_and(|proof| {
                    proof.bundle == bundle
                        && proof.root == state.proof_root()
                        && proof.fence == self.term.token
                        && proof.at >= state.metadata.fence_at
                        && proof.at <= now
                        && now - proof.at <= self.options.proof_lifetime
                        && (state.phase() != Phase::Switched || proof.drained)
                })
            });
            if all_proven {
                match state.phase() {
                    Phase::Stable => (),
                    Phase::Overlap => {
                        state.metadata.active = state.metadata.authorities[1].digest.clone();
                        state.metadata.phase = Phase::Switched;
                        state.next_generation()?;
                    }
                    Phase::Switched
                        if now
                            >= state.metadata.authorities[0]
                                .last_issued_expiry
                                .saturating_add(self.options.clock_skew) =>
                    {
                        state.metadata.authorities.remove(0);
                        state.metadata.phase = Phase::Stable;
                        state.next_generation()?;
                    }
                    Phase::Switched => (),
                }
            }
            phase = state.phase();
            self.persist(&snapshot, &state).await?;
        }
        self.publish().await?;
        Ok(phase)
    }

    /// Capture candidates BEFORE starting the direct, unfiltered namespace Pod
    /// list. The resulting observation is evidence only for these admissions.
    pub async fn retirement_candidates(&self) -> Result<RetirementCandidates> {
        let (_, state) = self.load().await?;
        Ok(RetirementCandidates {
            fence: self.term.token.clone(),
            identities: state
                .participants
                .members
                .into_iter()
                .map(|(key, member)| (key, member.identity))
                .collect(),
        })
    }

    /// The live set must come from a successful direct, unfiltered namespace Pod
    /// list started AFTER capturing candidates. Labels, termination timestamps,
    /// readiness and watch disappearance are not authoritative absence. Identity
    /// changes are conservatively deferred until a later sweep. The latest CAS
    /// preserves all concurrent admissions, leaf records and issuer watermarks.
    pub async fn retire_absent_pods(
        &self,
        candidates: RetirementCandidates,
        live_uids: &BTreeSet<String>,
    ) -> Result<()> {
        let _guard = self.writer.lock().await;
        let (snapshot, mut state) = self.load().await?;
        ensure!(
            candidates.fence == self.term.token,
            "retirement observation belongs to another term"
        );
        for (key, identity) in candidates.identities {
            if !live_uids.contains(&identity.pod_uid)
                && state
                    .member(&key)
                    .is_some_and(|member| member.identity == identity)
            {
                state.participants.members.remove(&key);
                state.participants.retired.insert(key);
            }
        }
        self.persist(&snapshot, &state).await
    }

    /// A pending placeholder can disappear only after this exact Pod's actual
    /// boot has supplied a current TLS proof. Actual old boots remain members.
    pub async fn retire_pending(&self, proven_key: &str, now: i64) -> Result<()> {
        let _guard = self.writer.lock().await;
        let (snapshot, mut state) = self.load().await?;
        let member = state.member(proven_key).context("unknown process")?;
        let proof = member.proof.as_ref().context("missing process proof")?;
        ensure!(
            member.identity.kind == IdentityKind::ControlPlane
                && member.identity.boot_id != "pending",
            "not an actual CP boot"
        );
        ensure!(
            proof.fence == self.term.token
                && proof.bundle == state.bundle().digest()
                && proof.at <= now
                && now - proof.at <= self.options.proof_lifetime,
            "stale CP proof"
        );
        let key = format!("{}/pending", member.identity.pod_uid);
        state.participants.members.remove(&key);
        state.participants.retired.insert(key);
        self.persist(&snapshot, &state).await
    }
}
