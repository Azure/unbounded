//! Racer publication validation and topology projection over generic immutable state.

use super::cell::{Generation, PublicationCell, Retention};
use super::{CacheTransition, Snapshot};
use crate::control::poll::{Document, Preparation, Prepared};
use crate::error::{Error, Result};
use crate::topology::{Member, Membership};
use racer_wire::control::{self as wire, ClusterId, Publication, PublicationSequence};
use sha2::{Digest, Sha256};
use std::{cell::RefCell, rc::Rc, sync::Arc, time::Duration};

/// The REST method surface cannot represent arbitrary string methods:
/// ```compile_fail
/// let method = crate::control::rest::Method::Delete;
/// ```
/// Cluster-bound projection and local cache installation policy, never a registry.
pub struct PublicationTarget {
    cluster: ClusterId,

    pub(crate) published: Arc<PublicationCell<Snapshot>>,

    lifecycle: RefCell<Option<Rc<crate::app::CachePublication>>>,
    /// Stamps accepted publications.
    clock: racer_io::Clock,
}

impl PublicationTarget {
    /// Bind projection to the node-wide publication authority.
    pub fn new(
        cluster: ClusterId,
        published: Arc<PublicationCell<Snapshot>>,
        clock: racer_io::Clock,
    ) -> Self {
        Self {
            cluster,
            published,
            lifecycle: RefCell::new(None),
            clock,
        }
    }

    /// Attach the control worker's cache rollout projection.
    pub(crate) fn attach_cache_publication(&self, lifecycle: Rc<crate::app::CachePublication>) {
        *self.lifecycle.borrow_mut() = Some(lifecycle);
    }

    /// Synchronously project a fixture using the production preparation closure.
    #[cfg(any(test, feature = "subscription-interop", feature = "dst"))]
    pub(crate) fn project(&self, document: Publication) -> Result<Arc<Snapshot>> {
        let job = self.projection(Arc::new(document))?;
        job()
    }

    /// Accept a domain projection and commit prepared local resources atomically.
    pub(super) fn accept(
        &self,
        snapshot: Arc<Snapshot>,
        transition: Option<Box<dyn CacheTransition + '_>>,
    ) -> Result<Arc<Snapshot>> {
        self.published.publish(
            snapshot,
            self.clock.now(),
            |_, next| {
                if next.cluster != self.cluster {
                    return Err(Error::Unauthorized);
                }
                Ok(())
            },
            || {
                if let Some(transition) = transition {
                    transition.commit();
                }
            },
        )
    }

    /// Publish fixture state without a serving listener lifecycle.
    #[cfg(any(test, feature = "subscription-interop", feature = "dst"))]
    pub(crate) fn apply(&self, document: Publication) -> Result<Arc<Snapshot>> {
        self.accept(self.project(document)?, None)
    }

    /// Project and accept a document with a caller-owned cache rollback guard.
    #[cfg(any(test, feature = "dst"))]
    pub(crate) fn apply_staged(
        &self,
        document: Publication,
        transition: Option<Box<dyn CacheTransition + '_>>,
    ) -> Result<Arc<Snapshot>> {
        self.accept(self.project(document)?, transition)
    }

    /// Require an accepted cluster view for local admission.
    pub fn current(&self) -> Result<Arc<Snapshot>> {
        self.published.current()?.ok_or(Error::Unavailable)
    }

    /// Report acceptance rather than the pending receipt cursor.
    pub fn cursor(&self) -> Result<Option<PublicationSequence>> {
        Ok(self.published.current()?.map(|s| s.sequence))
    }

    /// Stage the control worker's resources before atomically accepting the projection.
    pub(crate) fn install_projection(&self, prepared: &Arc<Snapshot>) -> Result<()> {
        let lifecycle = self.lifecycle.borrow();
        let transition = lifecycle
            .as_ref()
            .map(|l| l.stage_for(&prepared.caches, Some(&prepared.membership)))
            .transpose()?;
        self.accept(prepared.clone(), transition)?;
        Ok(())
    }
}

impl PublicationTarget {
    /// Capture thread-safe projection inputs; the returned closure does the CPU work.
    pub fn projection(
        &self,
        document: Arc<Publication>,
    ) -> Result<Box<dyn FnOnce() -> Result<Arc<Snapshot>> + Send + 'static>> {
        let cluster = self.cluster.clone();
        let predecessor = self.published.current()?;
        Ok(Box::new(move || {
            project(&cluster, &document, predecessor.as_deref())
        }))
    }

    /// Install a projection of exactly `document`.
    pub fn install_publication(
        &self,
        document: &Publication,
        prepared: &Arc<Snapshot>,
    ) -> Result<()> {
        if document.sequence != prepared.sequence || document.cluster != prepared.cluster {
            return Err(Error::InvalidRequest);
        }
        self.install_projection(prepared)
    }

    /// Poll install hook: prepare a publication document.
    pub(crate) fn prepare(&self, document: Document) -> Result<Preparation> {
        let Document::Publication(document) = document else {
            return Err(Error::InvalidRequest);
        };
        let job = self.projection(document)?;
        Ok(Box::new(move || job().map(Prepared::Snapshot)))
    }

    /// Poll install hook: install a prepared publication snapshot.
    pub(crate) fn install(&self, document: &Document, prepared: &Prepared) -> Result<()> {
        match (document, prepared) {
            (Document::Publication(document), Prepared::Snapshot(prepared)) => {
                self.install_publication(document, prepared)
            }
            _ => Err(Error::InvalidRequest),
        }
    }
}

impl Snapshot {
    /// Racer late-request policy; retention accounting belongs entirely to PublicationCell.
    pub fn retention(old_parts: usize) -> Retention {
        Retention {
            old_parts,
            grace_parts: if old_parts >= 2 { old_parts } else { 0 },
            grace_bytes: 128 * 1024 * 1024,
            grace: Duration::from_secs(30),
        }
    }

    /// Canonical wire digest used when requesting a delta.
    pub fn content_hash(&self) -> String {
        self.content_hash
            .iter()
            .map(|byte| format!("{byte:02x}"))
            .collect()
    }
}

impl Generation for Snapshot {
    type Version = PublicationSequence;

    type PartVersion = u64;

    type Part = Membership;

    type Digest = [u8; 32];

    fn version(&self) -> Self::Version {
        self.sequence
    }

    fn part_version(&self) -> Self::PartVersion {
        self.membership.version.0
    }

    fn part(&self) -> &Arc<Membership> {
        &self.membership
    }

    fn digest(&self) -> &Self::Digest {
        &self.content_hash
    }

    fn part_digest(&self) -> &Self::Digest {
        &self.membership_hash
    }

    fn part_bytes(part: &Membership) -> usize {
        part.retained_bytes()
    }
}

/// Validate domain invariants and reuse the accepted topology for cache-only updates.
fn project(
    cluster: &ClusterId,
    document: &Publication,
    current: Option<&Snapshot>,
) -> Result<Arc<Snapshot>> {
    if &document.cluster != cluster {
        return Err(Error::Unauthorized);
    }
    let publication = wire::validate_publication(document)?;
    // Stored pages and in-flight reads are split by the adopted page size; a node never
    // switches it under them.
    if current.is_some_and(|old| old.page_size != publication.page_size) {
        return Err(Error::InvalidConfiguration);
    }
    let (content, membership) = wire::canonical_content(&publication)?;
    let content_hash: [u8; 32] = Sha256::digest(content).into();
    let membership_hash: [u8; 32] = Sha256::digest(membership).into();
    // Replay and monotonicity are decided once, by `cell::accept`, when the
    // projection is published. Here only reuse the accepted topology.
    let membership = if let Some(old) =
        current.filter(|old| old.membership.version == publication.membership_version)
    {
        if membership_hash != old.membership_hash {
            return Err(Error::IncompatibleMembership);
        }
        old.membership.clone()
    } else {
        Arc::new(Membership::validate_with_pools(
            publication.membership_version,
            publication.members.into_iter().map(Member::from).collect(),
            publication.pools.clone(),
            current.map(|old| old.membership.as_ref()),
        )?)
    };
    Ok(Arc::new(Snapshot {
        cluster: publication.cluster,
        sequence: publication.sequence,
        membership,
        caches: Arc::new(publication.caches.into()),
        pools: publication.pools,
        page_size: publication.page_size,
        content_hash,
        membership_hash,
    }))
}
