//! Per-worker write directory: the write sessions this worker coordinates and
//! the manifests it keeps as a manifest replica.
//!
//! Each written object lives on the worker that owns its page zero
//! (`WorkerMap::metadata_owner`), the same worker that serves its metadata.
//! Fresh metadata refreshes consult this directory before the origin
//! ([`Directory::current`]), because the origin does not know an object
//! until its upload commits.

use crate::model::ExpiresAt;
use crate::model::ObjectId;
use crate::model::ObjectMetadata;
use crate::model::ObjectVersion;
use crate::runtime::HashMap;
use crate::security::OriginContext;
use racer_core::write::Phase;
use racer_core::write::Session;
use racer_core::write::UploadState;
use std::cell::RefCell;
use std::time::Duration;
use std::time::SystemTime;

/// How long a reader may reuse a written object's current-version pointer
/// before revalidating with its coordinator. Short, so a `DELETE` or an
/// upload becomes visible on every node within this bound.
pub const METADATA_TTL: Duration = Duration::from_secs(1);

/// Why this worker tracks an object.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Role {
    /// The page-zero owner that owns the session, upload, and deletes.
    Coordinator,
    /// A manifest replica: serves metadata if the coordinator is lost, and
    /// drops the entry at its hold deadline.
    Replica,
}

/// One written object version.
pub struct Entry {
    pub role: Role,

    pub version: ObjectVersion,

    pub session: Session,

    /// The request's origin context, retained in memory (never journaled)
    /// for the upload. `None` after a restart until a client request
    /// supplies it again.
    pub context: Option<OriginContext>,

    /// Upload attempts made by Put Block List.
    pub commit_attempts: u32,

    /// A repair task is restoring copies of this version's pages.
    pub repairing: bool,

    /// When this worker last decided its role for this entry, and under
    /// which membership version. `None` forces a
    /// check on the next maintenance turn.
    pub role_checked: Option<(u64, std::time::Instant)>,

    /// The membership version under which every page's rank-0 owner last
    /// got a copy (`repair::rebalance`). `None` until the first pass.
    pub rebalanced: Option<u64>,

    /// Holders seen missing from the cache's pool, with the wall time (Unix
    /// millis) they were first seen missing. A holder is dropped only after
    /// `RACER_WRITE_HOLDER_GRACE_SECS`; one seen again is forgotten.
    pub missing: std::collections::BTreeMap<racer_core::write::NodeIdBytes, u64>,

    /// Whether this worker may serve the version. A committed session
    /// restored from the journal starts unconfirmed: it may have missed a
    /// `DELETE` while the worker was down, so it serves no fresh read and
    /// takes no coordinator action until its role check finds that no
    /// page-zero member holds a tombstone for it
    /// (`failover::check_roles`).
    pub confirmed: bool,
}

impl Entry {
    /// An entry with no repair running and no role decided yet.
    pub fn new(
        role: Role,
        version: ObjectVersion,
        session: Session,
        context: Option<OriginContext>,
    ) -> Self {
        Self {
            role,
            version,
            session,
            context,
            commit_attempts: 0,
            repairing: false,
            role_checked: None,
            rebalanced: None,
            missing: Default::default(),
            confirmed: true,
        }
    }

    /// Immutable metadata with a reader TTL of [`METADATA_TTL`], capped at
    /// the hold deadline.
    pub fn metadata(&self, now: SystemTime) -> Option<ObjectMetadata> {
        let spec = self.session.spec();
        let hold = crate::store::writes::from_unix_millis(spec.hold_until_millis);
        // Whole milliseconds: `ExpiresAt` refuses finer times.
        let expires = crate::store::writes::from_unix_millis(crate::store::writes::unix_millis(
            (now + METADATA_TTL).min(hold),
        ));
        Some(ObjectMetadata {
            content_type: spec
                .content_type
                .as_deref()
                .and_then(|c| crate::model::ContentType::parse(c.as_bytes()).ok()),
            version: self.version.clone(),
            length: spec.length,
            expires_at: ExpiresAt::from_system_time(expires).ok()?,
        })
    }

    /// Whether readers should see this version as the object's current one:
    /// confirmed, committed, not uploaded (the origin is authoritative
    /// afterward), and inside its hold.
    pub fn visible(&self, now: SystemTime) -> bool {
        self.confirmed
            && matches!(self.session.phase(), Phase::Committed)
            && self.session.state() != UploadState::Committed
            && now < crate::store::writes::from_unix_millis(self.session.spec().hold_until_millis)
    }
}

/// A client-deleted version this worker remembers until its hold ends.
#[derive(Clone, Debug)]
pub struct Tombstone {
    /// The version's hold deadline (Unix millis): no copy serves past it.
    pub hold_until_millis: u64,
    /// The version's replica count: an acknowledged delete left the
    /// tombstone on at least this many page-zero members (or all of them,
    /// when fewer).
    pub replicas: u8,
    /// A background task is spreading it to page-zero members.
    pub spreading: bool,
    /// Background spreads that left a member without it, in a row, and
    /// when the next may start (exponential backoff).
    pub failures: u32,
    pub retry_at: Option<std::time::Instant>,
    /// The membership version under which every page-zero member of the
    /// object acknowledged this tombstone, if one did.
    pub spread: Option<u64>,
    /// Members that acknowledged it under the membership version `acked.0`.
    pub acked: (u64, std::collections::BTreeSet<racer_wire::control::NodeId>),
}

impl Tombstone {
    /// A tombstone nobody else acknowledged yet.
    pub fn new(hold_until_millis: u64, replicas: u8) -> Self {
        Self {
            hold_until_millis,
            replicas,
            spreading: false,
            failures: 0,
            retry_at: None,
            spread: None,
            acked: Default::default(),
        }
    }
}

/// The written objects of one worker.
#[derive(Default)]
pub struct Directory {
    entries: RefCell<HashMap<ObjectId, Entry>>,
    tombstones: RefCell<HashMap<ObjectVersion, Tombstone>>,
}

impl Directory {
    /// Whether `version` was deleted by a client and its tombstone is live.
    pub fn tombstoned(&self, version: &ObjectVersion) -> bool {
        self.tombstones.borrow().contains_key(version)
    }

    /// Whether `object` has an unconfirmed (restored, not yet checked for
    /// tombstones) committed session here. Fresh reads of it are retryable,
    /// not `404`, until it is confirmed or dropped.
    pub fn unconfirmed(&self, object: &ObjectId) -> bool {
        self.entries
            .borrow()
            .get(object)
            .is_some_and(|e| !e.confirmed && matches!(e.session.phase(), Phase::Committed))
    }

    /// Remember the tombstone of `version` (keeping the later hold).
    pub fn add_tombstone(&self, version: ObjectVersion, hold_until_millis: u64, replicas: u8) {
        let mut tombstones = self.tombstones.borrow_mut();
        let tombstone = tombstones
            .entry(version)
            .or_insert_with(|| Tombstone::new(hold_until_millis, replicas));
        tombstone.hold_until_millis = tombstone.hold_until_millis.max(hold_until_millis);
    }

    /// Run `f` on the tombstone of `version`.
    pub fn with_tombstone<R>(
        &self,
        version: &ObjectVersion,
        f: impl FnOnce(Option<&mut Tombstone>) -> R,
    ) -> R {
        f(self.tombstones.borrow_mut().get_mut(version))
    }

    /// Every live tombstone's version.
    pub fn tombstones(&self) -> Vec<ObjectVersion> {
        self.tombstones.borrow().keys().cloned().collect()
    }

    /// The tombstoned versions of `object`.
    pub fn tombstones_of(&self, object: &ObjectId) -> Vec<ObjectVersion> {
        self.tombstones
            .borrow()
            .keys()
            .filter(|v| v.object == *object)
            .cloned()
            .collect()
    }

    /// Drop tombstones whose hold ended at `now_millis`.
    pub fn expire_tombstones(&self, now_millis: u64) {
        self.tombstones
            .borrow_mut()
            .retain(|_, t| t.hold_until_millis > now_millis);
    }

    /// The current written version of `object` for a fresh refresh, if one is
    /// visible.
    pub fn current(&self, object: &ObjectId, now: SystemTime) -> Option<ObjectMetadata> {
        let entries = self.entries.borrow();
        let entry = entries.get(object)?;
        if !entry.visible(now) {
            return None;
        }
        entry.metadata(now)
    }

    /// [`Self::current`], with the role this worker tracks it in.
    pub fn current_role(
        &self,
        object: &ObjectId,
        now: SystemTime,
    ) -> Option<(ObjectMetadata, Role)> {
        let entries = self.entries.borrow();
        let entry = entries.get(object)?;
        if !entry.visible(now) {
            return None;
        }
        Some((entry.metadata(now)?, entry.role))
    }

    /// Run `f` on the entry of `object`.
    pub fn with<R>(&self, object: &ObjectId, f: impl FnOnce(Option<&mut Entry>) -> R) -> R {
        f(self.entries.borrow_mut().get_mut(object))
    }

    /// Insert `entry`, unless `object` already has one. Returns whether it
    /// was inserted.
    pub fn insert(&self, object: ObjectId, entry: Entry) -> bool {
        let mut entries = self.entries.borrow_mut();
        if entries.contains_key(&object) {
            return false;
        }
        entries.insert(object, entry);
        true
    }

    /// Replace any entry of `object` with `entry`.
    pub fn replace(&self, object: ObjectId, entry: Entry) {
        self.entries.borrow_mut().insert(object, entry);
    }

    /// Remove the entry of `object`.
    pub fn remove(&self, object: &ObjectId) -> Option<Entry> {
        self.entries.borrow_mut().remove(object)
    }

    /// Remove the entry of `object` only if it is the version `etag`.
    pub fn remove_version(&self, version: &ObjectVersion) -> Option<Entry> {
        let mut entries = self.entries.borrow_mut();
        if entries
            .get(&version.object)
            .is_some_and(|entry| entry.version == *version)
        {
            return entries.remove(&version.object);
        }
        None
    }

    /// Every tracked object.
    pub fn objects(&self) -> Vec<ObjectId> {
        self.entries.borrow().keys().cloned().collect()
    }

    /// Number of tracked objects.
    pub fn len(&self) -> usize {
        self.entries.borrow().len()
    }

    /// Whether nothing is tracked.
    pub fn is_empty(&self) -> bool {
        self.entries.borrow().is_empty()
    }

    /// Upload-state counts of coordinated, committed objects, per cache, for
    /// the `racer_write_upload_state` gauge.
    pub fn upload_states(&self) -> Vec<(String, UploadState)> {
        self.entries
            .borrow()
            .values()
            .filter(|e| e.role == Role::Coordinator && e.session.phase() == Phase::Committed)
            .map(|e| (e.version.object.cache.0.clone(), e.session.state()))
            .collect()
    }
}
