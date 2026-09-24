// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

use super::*;
use anyhow::{Context, bail};
use std::{
    future::Future,
    sync::{
        Arc,
        atomic::{AtomicBool, Ordering},
    },
};

const STATE_LIMIT: usize = 16 * 1024;

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum Phase {
    Stable,
    Overlap,
    Switched,
}

/// The entire durable PKI state. No field grows with fleet size or boot history.
#[derive(Clone, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct CaState {
    version: u32,
    namespace: String,
    fence: String,
    generation: u64,
    active: String,
    phase: Phase,
    authorities: Vec<Authority>,
    rotation_nonce: String,
    published_at: Option<i64>,
    overlap_delay: i64,
    retirement_skew: i64,
}

/// The bounded CA metadata persisted in one Secret.
#[derive(Clone, PartialEq, Eq)]
pub struct StateImage {
    pub metadata: Vec<u8>,
}

impl CaState {
    pub fn bundle(&self) -> TrustBundle {
        TrustBundle {
            version: 1,
            generation: self.generation,
            active: self.active.clone(),
            certificates: self
                .authorities
                .iter()
                .map(|a| a.certificate.as_str())
                .collect(),
        }
    }
    pub fn phase(&self) -> Phase {
        self.phase
    }
    pub fn fence(&self) -> &str {
        &self.fence
    }
    pub fn expiry_watermarks(&self) -> impl Iterator<Item = (&str, i64)> {
        self.authorities
            .iter()
            .map(|a| (a.digest.as_str(), a.last_issued_expiry))
    }
    pub fn proof_root(&self) -> &str {
        if self.phase == Phase::Overlap {
            &self.authorities[1].digest
        } else {
            &self.active
        }
    }
    pub fn published_at(&self) -> Option<i64> {
        self.published_at
    }
    pub fn to_image(&self) -> Result<StateImage> {
        let metadata = serde_json::to_vec(self)?;
        ensure!(metadata.len() <= STATE_LIMIT, "CA state capacity exceeded");
        Ok(StateImage { metadata })
    }
    pub fn from_image(image: &StateImage) -> Result<Self> {
        ensure!(
            image.metadata.len() <= STATE_LIMIT,
            "unsupported CA state image"
        );
        let state: Self = serde_json::from_slice(&image.metadata)?;
        ensure!(
            state.version == 1
                && !state.fence.is_empty()
                && state.fence.len() <= 256
                && state.generation > 0
                && state.rotation_nonce.len() <= 64
                && state.overlap_delay > 0
                && state.retirement_skew >= 0,
            "invalid CA metadata"
        );
        ensure!(
            super::certificates::valid_namespace(&state.namespace),
            "invalid CA namespace"
        );
        ensure!(
            state.authorities.len() == if state.phase == Phase::Stable { 1 } else { 2 },
            "invalid CA phase"
        );
        ensure!(
            state.active == state.authorities[usize::from(state.phase == Phase::Switched)].digest,
            "invalid active CA"
        );
        ensure!(
            state.published_at.is_none_or(|at| at > 0),
            "invalid publication timestamp"
        );
        ensure!(
            state.phase != Phase::Switched || state.published_at.is_some(),
            "issuer switched before publication"
        );
        for ca in &state.authorities {
            ca.parse()?;
        }
        TrustBundle::parse(&state.bundle().json())?;
        Ok(state)
    }
    fn next_generation(&mut self) -> Result<()> {
        self.generation = self
            .generation
            .checked_add(1)
            .context("trust generation exhausted")?;
        Ok(())
    }
    /// Local authorization evidence only. The caller additionally checks current
    /// topology selection, including namespace/name/Pod UID and route identity.
    pub fn verify_peer<'a>(&self, peer: &'a VerifiedPeer, now: i64) -> Result<&'a Identity> {
        peer.valid_at(now)?;
        ensure!(peer.namespace() == self.namespace, "TLS namespace mismatch");
        ensure!(
            self.authorities.iter().any(|a| a.digest == peer.root),
            "TLS issuer retired"
        );
        Ok(peer.identity())
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
    pub prior_artifacts: bool,
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum CommitOutcome {
    Committed,
    Conflict,
    Uncertain,
}

/// Authoritative reads and resourceVersion CAS. Publication captures the public
/// revision before checking the Secret fence, and never retries a stale write.
pub trait CaStore: Send + Sync {
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
    /// Minimum time after confirmed overlap publication before switching issuer.
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
        ensure!(
            !token.is_empty() && token.len() <= 256,
            "invalid leadership fence"
        );
        Ok(Self {
            token,
            alive: Arc::new(AtomicBool::new(true)),
        })
    }
    pub fn cancel(&self) {
        self.alive.store(false, Ordering::Release);
    }
    pub(super) fn check(&self) -> Result<()> {
        ensure!(self.is_active(), "leadership canceled");
        Ok(())
    }
}

pub struct CaManager<S> {
    store: S,
    term: Leadership,
    options: SecurityOptions,
    writer: tokio::sync::Mutex<()>,
    committed: std::sync::RwLock<Option<CaState>>,
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
    pub async fn collect(&self) -> Result<()> {
        self.term.check()
    }
    pub async fn acquire(
        store: S,
        term: Leadership,
        options: SecurityOptions,
        now: i64,
    ) -> Result<Self> {
        options.validate()?;
        term.check()?;
        ensure!(now > 0, "invalid clock");
        let manager = Self {
            store,
            term,
            options,
            writer: tokio::sync::Mutex::new(()),
            committed: std::sync::RwLock::new(None),
        };
        let snapshot = manager.store.read().await?;
        let state = if let Some(image) = &snapshot.image {
            ensure!(
                snapshot.resource_version.is_some(),
                "missing Secret revision"
            );
            let mut state = CaState::from_image(image)?;
            ensure!(
                state.namespace == manager.options.namespace,
                "CA namespace changed"
            );
            ensure!(state.fence != manager.term.token, "leadership token reused");
            state.fence.clone_from(&manager.term.token);
            // A successor may increase safety margins, never shorten old bounds.
            state.retirement_skew = state.retirement_skew.max(manager.options.clock_skew);
            state.overlap_delay = state.overlap_delay.max(manager.options.proof_lifetime);
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
                version: 1,
                namespace: manager.options.namespace.clone(),
                fence: manager.term.token.clone(),
                generation: 1,
                active: ca.digest.clone(),
                phase: Phase::Stable,
                authorities: vec![ca],
                rotation_nonce: String::new(),
                published_at: None,
                overlap_delay: manager.options.proof_lifetime,
                retirement_skew: manager.options.clock_skew,
            }
        };
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
        let state = CaState::from_image(snapshot.image.as_ref().context("lost CA state")?)?;
        ensure!(state.fence == self.term.token, "not the fenced leader");
        Ok((snapshot, state))
    }
    pub async fn state(&self) -> Result<CaState> {
        Ok(self.load().await?.1)
    }
    pub fn committed_state(&self) -> Result<CaState> {
        self.term.check()?;
        self.committed
            .read()
            .unwrap()
            .clone()
            .context("CA not loaded")
    }
    fn ready(&self, snapshot: &StoreSnapshot, state: &CaState) -> Result<()> {
        let p = snapshot
            .publication
            .as_ref()
            .context("trust not published")?;
        ensure!(
            p.fence == self.term.token && p.bytes == state.bundle().json(),
            "trust publication is not current"
        );
        Ok(())
    }
    async fn persist(&self, snapshot: &StoreSnapshot, state: &CaState) -> Result<()> {
        self.term.check()?;
        let image = state.to_image()?;
        ensure!(
            self.store.commit(snapshot, &image).await? != CommitOutcome::Conflict,
            "CA CAS conflict"
        );
        let observed = self.store.read().await?;
        let actual = observed.image.as_ref().context("CA commit not durable")?;
        CaState::from_image(actual)?;
        ensure!(actual == &image, "CA commit unresolved or leadership lost");
        self.term.check()?;
        *self.committed.write().unwrap() = Some(state.clone());
        Ok(())
    }
    pub async fn publish(&self) -> Result<()> {
        let _guard = self.writer.lock().await;
        let (snapshot, state) = self.load().await?;
        let bytes = state.bundle().json();
        if let Some(old) = &snapshot.publication {
            let bundle = TrustBundle::parse(&old.bytes)?;
            ensure!(bundle.generation <= state.generation, "trust rollback");
            ensure!(
                bundle.generation != state.generation || old.bytes == bytes,
                "trust equivocation"
            );
        }
        ensure!(
            self.store
                .publish(&snapshot, &self.term.token, &bytes)
                .await?
                != CommitOutcome::Conflict,
            "trust CAS conflict"
        );
        let (observed, mut current) = self.load().await?;
        ensure!(
            current.bundle().json() == bytes,
            "publication raced state transition"
        );
        self.ready(&observed, &current)?;
        if current.phase == Phase::Overlap && current.published_at.is_none() {
            // Start the delay only AFTER a successful authoritative observation.
            // A crash before this CAS merely restarts the delay conservatively.
            current.published_at = Some(unix_now());
            self.persist(&observed, &current).await?;
        }
        Ok(())
    }
    /// Admission is intentionally stateless; live authorization belongs to the
    /// enrollment adapter and must be repeated for every issuance.
    pub async fn admit(&self, identity: Identity) -> Result<()> {
        self.term.check()?;
        SignedClaims {
            version: 1,
            namespace: self.options.namespace.clone(),
            identity,
        }
        .validate()
    }
    pub async fn issue(
        &self,
        csr: &[u8],
        identity: Identity,
        probe: bool,
        now: i64,
    ) -> Result<IssuedCertificate> {
        let key = validate_csr(csr)?;
        let _guard = self.writer.lock().await;
        let (snapshot, mut state) = self.load().await?;
        self.ready(&snapshot, &state)?;
        let root = if probe {
            state.proof_root()
        } else {
            &state.active
        }
        .to_owned();
        let ca = state
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
        // No leaf can escape before the exact bounded expiry authorization is
        // committed and read back. Delayed predecessors cannot extend it after
        // takeover; an uncertain successful issuance remains covered forever.
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
        ensure!(nonce.len() <= 4096, "rotation nonce too long");
        let nonce = if nonce.is_empty() {
            String::new()
        } else {
            digest(nonce.as_bytes())
        };
        {
            let _guard = self.writer.lock().await;
            let (snapshot, mut state) = self.load().await?;
            self.ready(&snapshot, &state)?;
            if state.phase == Phase::Stable && (nonce.is_empty() || state.rotation_nonce != nonce) {
                if !nonce.is_empty() {
                    state.rotation_nonce = nonce;
                }
                state.authorities.push(Authority::generate(
                    now,
                    self.options.ca_lifetime,
                    self.options.clock_skew,
                )?);
                state.phase = Phase::Overlap;
                state.published_at = None;
                state.next_generation()?;
                self.persist(&snapshot, &state).await?;
            }
        }
        self.publish().await
    }
    /// Retained as a diagnostic endpoint only. No fleet state or rotation credit.
    pub async fn record_proof(&self, key: &str, proof: TlsProof, now: i64) -> Result<()> {
        self.term.check()?;
        let state = self.committed_state()?;
        let identity = state.verify_peer(&proof.peer, now)?;
        ensure!(
            identity.key() == key && proof.fence == self.term.token,
            "proof identity or term mismatch"
        );
        ensure!(
            proof.bundle == state.bundle().digest()
                && proof.ack.digest == proof.bundle
                && proof.ack.generation == state.generation
                && proof.at <= now
                && now - proof.at <= self.options.proof_lifetime
                && hex_id(&proof.session)
                && state
                    .authorities
                    .iter()
                    .any(|a| a.digest == proof.local_root),
            "stale proof"
        );
        Ok(())
    }
    pub async fn advance_rotation(&self, now: i64) -> Result<Phase> {
        let phase;
        {
            let _guard = self.writer.lock().await;
            let (snapshot, mut state) = self.load().await?;
            self.ready(&snapshot, &state)?;
            let old = state.phase;
            match state.phase {
                Phase::Overlap
                    if state.published_at.is_some_and(|at| {
                        now >= at
                            .saturating_add(state.overlap_delay)
                            .saturating_add(state.retirement_skew)
                    }) =>
                {
                    state.active = state.authorities[1].digest.clone();
                    state.phase = Phase::Switched;
                }
                Phase::Switched
                    if now
                        >= state.authorities[0]
                            .last_issued_expiry
                            .saturating_add(state.retirement_skew) =>
                {
                    state.authorities.remove(0);
                    state.phase = Phase::Stable;
                    state.published_at = None;
                }
                _ => (),
            }
            phase = state.phase;
            if phase != old {
                state.next_generation()?;
                self.persist(&snapshot, &state).await?;
            }
        }
        self.publish().await?;
        Ok(phase)
    }
}
