//! Racer publication validation and topology projection over generic immutable state.

use super::{CacheTransition, Snapshot};
use crate::error::{Error, Result};
use crate::topology::{Member, Membership};
use controlplane::{Codec, Published, Target};
use racer_control_wire::{self as wire, ClusterId, Publication, PublicationSequence};
use sha2::{Digest, Sha256};
use std::{cell::RefCell, rc::Rc, sync::Arc, time::Duration};

/// Wire publication codec. The exact document remains available as a delta base.
/// The REST method surface cannot represent arbitrary string methods:
/// ```compile_fail
/// let method = wire_codec::rest::Method::Delete;
/// ```
pub struct PublicationCodec;

impl Codec for PublicationCodec {
    type Document = Publication;

    type Version = u64;

    type Error = Error;

    fn version(&self, document: &Publication) -> u64 {
        document.sequence.0
    }

    fn decode(&self, bytes: &[u8]) -> Result<Publication> {
        Ok(wire::decode_publication(bytes)?)
    }

    fn delta(&self, base: &Publication, bytes: &[u8]) -> Result<Publication> {
        Ok(wire::apply_delta(base, bytes)?)
    }

    fn digest(&self, document: &Publication) -> Result<String> {
        Ok(wire::content_hash(document)?)
    }
}

/// Cluster-bound projection and local cache installation policy, never a registry.
pub struct PublicationTarget {
    cluster: ClusterId,

    pub(crate) published: Arc<Published<Snapshot>>,

    lifecycle: RefCell<Option<Rc<crate::app::CachePublication>>>,
}

impl PublicationTarget {
    /// Bind projection to the node-wide publication authority.
    pub fn new(cluster: ClusterId, published: Arc<Published<Snapshot>>) -> Self {
        Self {
            cluster,
            published,
            lifecycle: RefCell::new(None),
        }
    }

    /// Attach the control worker's cache rollout projection.
    pub(crate) fn attach_cache_publication(&self, lifecycle: Rc<crate::app::CachePublication>) {
        *self.lifecycle.borrow_mut() = Some(lifecycle);
    }

    /// Synchronously project a fixture using the production preparation closure.
    #[cfg(any(test, feature = "subscription-interop"))]
    pub(crate) fn project(&self, document: Publication) -> Result<Arc<Snapshot>> {
        let job = <Self as Target<PublicationCodec>>::prepare(self, Arc::new(document))?;
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
            uring_runtime::environment::now(),
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
    #[cfg(any(test, feature = "subscription-interop"))]
    pub(crate) fn apply(&self, document: Publication) -> Result<Arc<Snapshot>> {
        self.accept(self.project(document)?, None)
    }

    /// Project and accept a document with a caller-owned cache rollback guard.
    #[cfg(test)]
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
            .map(|l| l.stage(&prepared.caches))
            .transpose()?;
        self.accept(prepared.clone(), transition)?;
        Ok(())
    }
}

impl Target<PublicationCodec> for PublicationTarget {
    type Prepared = Arc<Snapshot>;

    fn prepare(
        &self,
        document: Arc<Publication>,
    ) -> Result<controlplane::feed::Preparation<Self::Prepared, Error>> {
        let cluster = self.cluster.clone();
        let predecessor = self.published.current()?;
        Ok(Box::new(move || {
            project(&cluster, &document, predecessor.as_deref())
        }))
    }

    fn install(&self, document: &Publication, prepared: &Arc<Snapshot>) -> Result<()> {
        if document.sequence != prepared.sequence || document.cluster != prepared.cluster {
            return Err(Error::InvalidRequest);
        }
        self.install_projection(prepared)
    }
}

impl Snapshot {
    /// Racer late-request policy; retention accounting belongs entirely to Published.
    pub fn retention(old_parts: usize) -> controlplane::Retention {
        controlplane::Retention {
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

impl controlplane::Generation for Snapshot {
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
    let (content, membership) = wire::canonical_content(&publication)?;
    let content_hash: [u8; 32] = Sha256::digest(content).into();
    let membership_hash: [u8; 32] = Sha256::digest(membership).into();
    if let Some(old) = current
        && (publication.sequence < old.sequence
            || publication.membership_version.0 < old.membership.version.0
            || publication.sequence == old.sequence
                && (content_hash != old.content_hash
                    || publication.membership_version != old.membership.version))
    {
        return Err(Error::Replay);
    }
    let membership = if let Some(old) =
        current.filter(|old| old.membership.version == publication.membership_version)
    {
        if membership_hash != old.membership_hash {
            return Err(Error::IncompatibleMembership);
        }
        old.membership.clone()
    } else {
        Arc::new(Membership::validate_with_predecessor(
            publication.membership_version,
            publication.members.into_iter().map(Member::from).collect(),
            current.map(|old| old.membership.as_ref()),
        )?)
    };
    Ok(Arc::new(Snapshot {
        cluster: publication.cluster,
        sequence: publication.sequence,
        membership,
        caches: publication.caches,
        content_hash,
        membership_hash,
    }))
}
