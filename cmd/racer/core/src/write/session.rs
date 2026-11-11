//! Coordinator write-session state machine. Inputs are events, outputs are
//! effects; the caller performs the IO and feeds back results.
//!
//! Lifecycle: `Open` -> `Committed` (published) or `Aborted`. A committed
//! session moves through upload states and ends `Deleted` or `Expired`.
//! Every rejected event yields exactly one [`SessionEffect::Reject`] and
//! changes nothing.

use super::journal::JournalRecord;
use super::upload::{MAX_UPLOAD_BATCH, UploadPlan};
use super::{Holders, Manifest, NodeIdBytes, SessionId, UploadMode, UploadState, WriteSpec};

/// The object and version a session writes.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct WriteIdentity {
    pub cache: String,

    pub key: [u8; 32],

    /// Quoted Racer version tag.
    pub etag: String,

    pub session: SessionId,
}

/// Input to [`Session::on_event`].
#[derive(Clone, Debug, Eq, PartialEq)]
pub enum SessionEvent {
    /// The writer renewed its lease.
    Renewed,
    /// The writer committed with this holder table.
    Committed(Holders),
    /// The writer aborted.
    Aborted,
    /// Periodic timer: aborts an open session whose lease passed.
    LeaseTick,
    /// A holder left the membership or reported its copy lost.
    HolderLost {
        page: u64,
        node: NodeIdBytes,
    },
    /// Repair stored a new copy of `page` on `node`.
    HolderRestored {
        page: u64,
        node: NodeIdBytes,
    },
    /// Every holder in this list left the membership. One journal record for
    /// the whole loss, unlike per-page [`Self::HolderLost`].
    NodesLost(Vec<NodeIdBytes>),
    /// Add these holders (a repair batch, or another coordinator's table) to
    /// the table. Pages outside the object are ignored.
    HoldersMerged(Holders),
    PageUploaded(u64),
    /// Another coordinator of this version (after a failover) committed the
    /// block list. Valid in any committed upload state.
    UploadedElsewhere {
        blob_etag: Option<String>,
    },
    BlockListCommitted {
        blob_etag: String,
    },
    /// Put Block List found an existing blob.
    BlockListConflict,
    /// Upload gave up after its retries.
    UploadFailed,
    PersistRequested,
    /// Restart an upload that was pending when the coordinator restarted
    /// (`from_manifest` turns an interrupted upload into `Pending`). Valid only
    /// in `Pending`.
    UploadResumed,
    DeleteRequested,
    HoldExpired,
}

/// Why an event was refused.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Rejection {
    /// The lease ended before the event; the session is now aborted.
    LeaseExpired,
    /// Commit holders are missing pages, short of replicas, or duplicated.
    InvalidHolders,
    /// The event needs an open (uncommitted) session.
    NotOpen,
    /// The event needs a committed session.
    NotCommitted,
    /// `Persist` outside the `Manual` state.
    NotManual,
    /// `Delete` of an object already uploaded to the origin.
    Uploaded,
    /// The event does not apply to the current upload state.
    InvalidState,
    /// The session already ended.
    Terminal,
}

/// Output of [`Session::on_event`], to be applied in order.
#[derive(Clone, Debug, Eq, PartialEq)]
pub enum SessionEffect {
    /// Append and fsync before applying later effects.
    Journal(JournalRecord),
    /// Send `WriteRelease` to these nodes.
    Release { nodes: Vec<NodeIdBytes> },
    /// Send `WriteRelease` to every ranked store target of every page: before
    /// commit the coordinator does not know which targets stored pages.
    ReleaseTargets,
    /// Send `WriteUpload` for these pages to this holder.
    Upload { node: NodeIdBytes, pages: Vec<u64> },
    /// Send Put Block List.
    CommitBlockList,
    /// Store a new copy of `page` sourced from `from`.
    Restore { page: u64, from: NodeIdBytes },
    /// Publish the version descriptor and current pointer.
    Publish,
    /// Remove the current pointer.
    Unpublish,
    /// Wake status waiters with the new upload state.
    Notify(UploadState),
    /// The event was refused.
    Reject(Rejection),
}

/// Session lifecycle phase.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Phase {
    Open { lease_until_millis: u64 },
    Committed,
    Aborted,
    Deleted,
    Expired,
}

/// Coordinator state for one written version.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct Session {
    identity: WriteIdentity,
    spec: WriteSpec,
    lease_millis: u64,
    phase: Phase,
    holders: Holders,
    state: UploadState,
    uploaded: Vec<bool>,
    block_list_sent: bool,
    blob_etag: Option<String>,
}

impl Session {
    /// Open a session. The caller journals [`Self::open_record`] before replying.
    /// `spec` must already pass [`WriteSpec::validate`].
    pub fn open(
        identity: WriteIdentity,
        spec: WriteSpec,
        now_millis: u64,
        lease_millis: u64,
    ) -> Self {
        let pages = spec.pages() as usize;
        Self {
            identity,
            state: UploadState::initial(spec.upload),
            spec,
            lease_millis,
            phase: Phase::Open {
                lease_until_millis: now_millis.saturating_add(lease_millis),
            },
            holders: Holders::default(),
            uploaded: vec![false; pages],
            block_list_sent: false,
            blob_etag: None,
        }
    }

    /// Rebuild a committed session from a journaled or replicated manifest.
    /// An interrupted upload restarts from scratch; the caller replans it.
    pub fn from_manifest(
        cache: String,
        key: [u8; 32],
        manifest: Manifest,
        lease_millis: u64,
    ) -> Self {
        let pages = manifest.spec.pages() as usize;
        let state = match manifest.state {
            UploadState::Uploading => UploadState::Pending,
            state => state,
        };
        Self {
            identity: WriteIdentity {
                cache,
                key,
                etag: manifest.etag,
                session: manifest.session,
            },
            spec: manifest.spec,
            lease_millis,
            phase: Phase::Committed,
            holders: manifest.holders,
            state,
            uploaded: vec![false; pages],
            block_list_sent: false,
            blob_etag: manifest.blob_etag,
        }
    }

    pub fn identity(&self) -> &WriteIdentity {
        &self.identity
    }

    pub fn spec(&self) -> &WriteSpec {
        &self.spec
    }

    pub fn phase(&self) -> Phase {
        self.phase
    }

    pub fn state(&self) -> UploadState {
        self.state
    }

    pub fn holders(&self) -> &Holders {
        &self.holders
    }

    pub fn blob_etag(&self) -> Option<&str> {
        self.blob_etag.as_deref()
    }

    /// Lease deadline while open.
    pub fn lease_until_millis(&self) -> Option<u64> {
        match self.phase {
            Phase::Open { lease_until_millis } => Some(lease_until_millis),
            _ => None,
        }
    }

    /// Whether the session ended (aborted, deleted, or expired).
    pub fn is_terminal(&self) -> bool {
        matches!(self.phase, Phase::Aborted | Phase::Deleted | Phase::Expired)
    }

    /// Pages that have at least one but fewer than `replicas` holders, with
    /// their first surviving holder, while the object still needs copies
    /// (committed, not uploaded, not failed). Repair restores these.
    pub fn under_replicated(&self) -> Vec<(u64, NodeIdBytes)> {
        if self.phase != Phase::Committed
            || matches!(self.state, UploadState::Committed | UploadState::Failed)
        {
            return Vec::new();
        }
        let want = usize::from(self.spec.replicas);
        self.holders
            .0
            .iter()
            .enumerate()
            .filter(|(_, nodes)| !nodes.is_empty() && nodes.len() < want)
            .map(|(page, nodes)| (page as u64, nodes[0].clone()))
            .collect()
    }

    /// The `Open` journal record for this session.
    pub fn open_record(&self) -> JournalRecord {
        JournalRecord::Open {
            cache: self.identity.cache.clone(),
            key: self.identity.key,
            etag: self.identity.etag.clone(),
            session: self.identity.session,
            spec: self.spec.clone(),
            lease_until_millis: self.lease_until_millis().unwrap_or(0),
        }
    }

    /// The current manifest.
    pub fn manifest(&self) -> Manifest {
        Manifest {
            session: self.identity.session,
            etag: self.identity.etag.clone(),
            spec: self.spec.clone(),
            holders: self.holders.clone(),
            state: self.state,
            blob_etag: self.blob_etag.clone(),
        }
    }

    /// Apply one event.
    pub fn on_event(&mut self, event: SessionEvent, now_millis: u64) -> Vec<SessionEffect> {
        if self.is_terminal() {
            return match event {
                SessionEvent::LeaseTick | SessionEvent::HoldExpired => Vec::new(),
                _ => vec![SessionEffect::Reject(Rejection::Terminal)],
            };
        }
        if let Phase::Open { lease_until_millis } = self.phase {
            if now_millis > lease_until_millis {
                let mut effects = self.abort();
                if !matches!(event, SessionEvent::LeaseTick | SessionEvent::HoldExpired) {
                    effects.push(SessionEffect::Reject(Rejection::LeaseExpired));
                }
                return effects;
            }
            return self.on_open(event, now_millis);
        }
        self.on_committed(event)
    }

    fn on_open(&mut self, event: SessionEvent, now_millis: u64) -> Vec<SessionEffect> {
        match event {
            SessionEvent::Renewed => {
                self.phase = Phase::Open {
                    lease_until_millis: now_millis.saturating_add(self.lease_millis),
                };
                Vec::new()
            }
            SessionEvent::Committed(holders) => {
                if holders
                    .validate(self.spec.pages(), self.spec.replicas)
                    .is_err()
                {
                    return vec![SessionEffect::Reject(Rejection::InvalidHolders)];
                }
                self.holders = holders;
                self.phase = Phase::Committed;
                let mut effects = vec![
                    SessionEffect::Journal(self.commit_record()),
                    SessionEffect::Publish,
                    SessionEffect::Notify(self.state),
                ];
                if self.spec.upload == UploadMode::Auto {
                    effects.extend(self.start_upload());
                }
                effects
            }
            SessionEvent::Aborted => self.abort(),
            SessionEvent::DeleteRequested => {
                self.phase = Phase::Deleted;
                vec![
                    SessionEffect::Journal(self.record(Kind::Delete)),
                    SessionEffect::ReleaseTargets,
                ]
            }
            SessionEvent::LeaseTick | SessionEvent::HoldExpired => Vec::new(),
            _ => vec![SessionEffect::Reject(Rejection::NotCommitted)],
        }
    }

    fn on_committed(&mut self, event: SessionEvent) -> Vec<SessionEffect> {
        let uploaded = self.state == UploadState::Committed;
        match event {
            SessionEvent::Renewed | SessionEvent::Committed(_) | SessionEvent::Aborted => {
                vec![SessionEffect::Reject(Rejection::NotOpen)]
            }
            SessionEvent::LeaseTick => Vec::new(),
            SessionEvent::HolderLost { page, node } => {
                let Some(nodes) = self.holders.0.get_mut(page as usize) else {
                    return vec![SessionEffect::Reject(Rejection::InvalidState)];
                };
                // A page's last holder is never dropped: a missing node may
                // come back with its copy, so the page waits for it (reads
                // and uploads of it fail retryably meanwhile) instead of the
                // object failing.
                if !nodes.contains(&node) || nodes.len() == 1 {
                    return Vec::new();
                }
                nodes.retain(|n| *n != node);
                let from = nodes[0].clone();
                let mut effects = vec![SessionEffect::Journal(self.commit_record())];
                if !uploaded && self.state != UploadState::Failed {
                    effects.push(SessionEffect::Restore { page, from });
                }
                effects
            }
            SessionEvent::NodesLost(lost) => {
                let mut changed = false;
                let mut restore = Vec::new();
                for (page, nodes) in self.holders.0.iter_mut().enumerate() {
                    // Keep every holder of a page none of whose holders
                    // survive; see `HolderLost`.
                    if nodes.iter().all(|n| lost.contains(n)) {
                        continue;
                    }
                    let before = nodes.len();
                    nodes.retain(|n| !lost.contains(n));
                    if nodes.len() == before {
                        continue;
                    }
                    changed = true;
                    restore.push(SessionEffect::Restore {
                        page: page as u64,
                        from: nodes[0].clone(),
                    });
                }
                if !changed {
                    return Vec::new();
                }
                let mut effects = vec![SessionEffect::Journal(self.commit_record())];
                if !uploaded && self.state != UploadState::Failed {
                    effects.extend(restore);
                }
                effects
            }
            SessionEvent::HoldersMerged(extra) => {
                let mut changed = false;
                for (page, add) in extra.0.into_iter().enumerate() {
                    let Some(nodes) = self.holders.0.get_mut(page) else {
                        break;
                    };
                    for node in add {
                        if !node.is_empty()
                            && !nodes.contains(&node)
                            && nodes.len() < usize::from(u8::MAX)
                        {
                            nodes.push(node);
                            changed = true;
                        }
                    }
                }
                if !changed {
                    return Vec::new();
                }
                vec![SessionEffect::Journal(self.commit_record())]
            }
            SessionEvent::UploadedElsewhere { blob_etag } => {
                if uploaded {
                    return Vec::new();
                }
                self.state = UploadState::Committed;
                self.uploaded.iter_mut().for_each(|u| *u = true);
                self.block_list_sent = true;
                self.blob_etag = blob_etag;
                vec![
                    SessionEffect::Journal(self.state_record()),
                    SessionEffect::Release {
                        nodes: self.holders.nodes(),
                    },
                    SessionEffect::Notify(self.state),
                ]
            }
            SessionEvent::HolderRestored { page, node } => {
                let Some(nodes) = self.holders.0.get_mut(page as usize) else {
                    return vec![SessionEffect::Reject(Rejection::InvalidState)];
                };
                if nodes.contains(&node) || nodes.len() >= usize::from(u8::MAX) {
                    return Vec::new();
                }
                nodes.push(node);
                vec![SessionEffect::Journal(self.commit_record())]
            }
            SessionEvent::PageUploaded(page) => {
                if self.state != UploadState::Uploading {
                    return vec![SessionEffect::Reject(Rejection::InvalidState)];
                }
                let Some(slot) = self.uploaded.get_mut(page as usize) else {
                    return vec![SessionEffect::Reject(Rejection::InvalidState)];
                };
                *slot = true;
                if !self.block_list_sent && self.uploaded.iter().all(|&u| u) {
                    self.block_list_sent = true;
                    return vec![SessionEffect::CommitBlockList];
                }
                Vec::new()
            }
            SessionEvent::BlockListCommitted { blob_etag } => {
                if self.state != UploadState::Uploading || !self.block_list_sent {
                    return vec![SessionEffect::Reject(Rejection::InvalidState)];
                }
                self.state = UploadState::Committed;
                self.blob_etag = Some(blob_etag);
                vec![
                    SessionEffect::Journal(self.state_record()),
                    SessionEffect::Release {
                        nodes: self.holders.nodes(),
                    },
                    SessionEffect::Notify(self.state),
                ]
            }
            SessionEvent::BlockListConflict | SessionEvent::UploadFailed => {
                if self.state != UploadState::Uploading {
                    return vec![SessionEffect::Reject(Rejection::InvalidState)];
                }
                self.fail()
            }
            SessionEvent::PersistRequested => {
                if self.state != UploadState::Manual {
                    return vec![SessionEffect::Reject(Rejection::NotManual)];
                }
                self.start_upload()
            }
            SessionEvent::UploadResumed => {
                if self.state != UploadState::Pending {
                    return vec![SessionEffect::Reject(Rejection::InvalidState)];
                }
                self.start_upload()
            }
            SessionEvent::DeleteRequested => {
                if uploaded {
                    return vec![SessionEffect::Reject(Rejection::Uploaded)];
                }
                self.phase = Phase::Deleted;
                self.end()
            }
            SessionEvent::HoldExpired => {
                // A started upload finishes; an uploaded object is already released.
                if matches!(self.state, UploadState::Uploading | UploadState::Committed) {
                    return Vec::new();
                }
                self.phase = Phase::Expired;
                self.end()
            }
        }
    }

    fn abort(&mut self) -> Vec<SessionEffect> {
        self.phase = Phase::Aborted;
        vec![
            SessionEffect::Journal(self.record(Kind::Abort)),
            SessionEffect::ReleaseTargets,
        ]
    }

    fn end(&mut self) -> Vec<SessionEffect> {
        vec![
            SessionEffect::Journal(self.record(Kind::Delete)),
            SessionEffect::Release {
                nodes: self.holders.nodes(),
            },
            SessionEffect::Unpublish,
        ]
    }

    fn fail(&mut self) -> Vec<SessionEffect> {
        self.state = UploadState::Failed;
        vec![
            SessionEffect::Journal(self.state_record()),
            SessionEffect::Notify(self.state),
        ]
    }

    fn start_upload(&mut self) -> Vec<SessionEffect> {
        self.state = UploadState::Uploading;
        self.uploaded.iter_mut().for_each(|u| *u = false);
        self.block_list_sent = false;
        let mut effects = vec![
            SessionEffect::Journal(self.state_record()),
            SessionEffect::Notify(self.state),
        ];
        let plan = UploadPlan::new(
            &self.holders,
            0..self.spec.pages(),
            |_| true,
            MAX_UPLOAD_BATCH,
        );
        effects.extend(
            plan.batches
                .into_iter()
                .map(|(node, pages)| SessionEffect::Upload { node, pages }),
        );
        effects
    }

    fn commit_record(&self) -> JournalRecord {
        JournalRecord::Commit {
            cache: self.identity.cache.clone(),
            key: self.identity.key,
            manifest: self.manifest(),
        }
    }

    fn state_record(&self) -> JournalRecord {
        JournalRecord::State {
            cache: self.identity.cache.clone(),
            key: self.identity.key,
            etag: self.identity.etag.clone(),
            state: self.state,
            blob_etag: self.blob_etag.clone(),
        }
    }

    fn record(&self, kind: Kind) -> JournalRecord {
        let cache = self.identity.cache.clone();
        let key = self.identity.key;
        let etag = self.identity.etag.clone();
        match kind {
            Kind::Abort => JournalRecord::Abort { cache, key, etag },
            Kind::Delete => JournalRecord::Delete { cache, key, etag },
        }
    }
}

enum Kind {
    Abort,
    Delete,
}

#[cfg(test)]
mod tests;
