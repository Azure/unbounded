//! Racer publication validation and topology projection over generic immutable state.

use super::cell::{Generation, PublicationCell, Retention};
use super::{CacheTransition, Snapshot};
use crate::control::poll::{Document, Preparation, Prepared};
use crate::error::{Error, Result};
use crate::topology::{Member, Membership};
use racer_wire::control::{
    self as wire, CacheDefinition, ClusterId, Publication, PublicationSequence,
};
use racer_wire::object::PageSize;
use sha2::{Digest, Sha256};
use std::{cell::RefCell, cmp::Ordering, rc::Rc, sync::Arc, time::Duration};

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
            |old, next| {
                if next.cluster != self.cluster {
                    return Err(Error::Unauthorized);
                }
                // Checked again under the cell lock: the projection compared
                // against the snapshot current when it was prepared.
                if let Some(old) = old
                    && (old.page_size != next.page_size
                        || !page_sizes_unchanged(
                            &old.caches,
                            old.page_size,
                            &next.caches,
                            next.page_size,
                        ))
                {
                    return Err(Error::InvalidConfiguration);
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

/// Whether every cache present in both sorted catalogs keeps its effective
/// page size. A cache removed or introduced under a new UID may use any size;
/// a cache that remains published never changes the size its pages were cut at.
fn page_sizes_unchanged(
    old: &[CacheDefinition],
    old_default: PageSize,
    next: &[CacheDefinition],
    next_default: PageSize,
) -> bool {
    let (mut i, mut j) = (0, 0);
    while i < old.len() && j < next.len() {
        match old[i].id.cmp(&next[j].id) {
            Ordering::Less => i += 1,
            Ordering::Greater => j += 1,
            Ordering::Equal => {
                if old[i].page_size.unwrap_or(old_default)
                    != next[j].page_size.unwrap_or(next_default)
                {
                    return false;
                }
                i += 1;
                j += 1;
            }
        }
    }
    true
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
    // switches it under a cache that remains published.
    if let Some(old) = current
        && (old.page_size != publication.page_size
            || !page_sizes_unchanged(
                &old.caches,
                old.page_size,
                &publication.caches,
                publication.page_size,
            ))
    {
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

#[cfg(test)]
mod tests {
    use super::*;
    use racer_wire::control::CacheId;

    const CACHE_A: &str = "44444444-4444-4444-8444-444444444444";
    const CACHE_B: &str = "55555555-5555-4555-8555-555555555555";
    const CACHE_C: &str = "66666666-6666-4666-8666-666666666666";
    const SMALL: u64 = 64 << 10;
    const MEDIUM: u64 = 128 << 10;

    /// Fixture publication with matching sequence and membership counters.
    fn publication(sequence: u64) -> Publication {
        let mut p = wire::decode_publication(include_bytes!(concat!(
            env!("CARGO_MANIFEST_DIR"),
            "/wire/testdata/publication.json"
        )))
        .unwrap();
        p.sequence.0 = sequence;
        p.membership_version.0 = sequence;
        p
    }

    fn new_target(env: &racer_io::Env) -> PublicationTarget {
        PublicationTarget::new(
            publication(1).cluster,
            Arc::new(PublicationCell::new(Snapshot::retention(2))),
            env.ctx.clock.clone(),
        )
    }

    fn sized(mut p: Publication, size: Option<PageSize>) -> Publication {
        p.caches[0].page_size = size;
        p
    }

    /// Append a cache with its own page size and extend the default pool.
    fn add_cache(p: &mut Publication, id: &str, name: &str, size: Option<PageSize>) {
        let mut cache = p.caches[0].clone();
        cache.id = CacheId(id.into());
        cache.name = name.into();
        (cache.client_socket, cache.origin_socket) = wire::canonical_socket_paths(name).unwrap();
        cache.page_size = size;
        p.caches.push(cache);
        p.caches.sort_by(|a, b| a.id.cmp(&b.id));
        p.pools[0].caches.push(CacheId(id.into()));
        p.pools[0].caches.sort();
    }

    fn remove_cache(p: &mut Publication, id: &str) {
        let id = CacheId(id.into());
        p.caches.retain(|cache| cache.id != id);
        p.pools[0].caches.retain(|cache| cache != &id);
    }

    /// Projection refuses to split one cache's stored pages between sizes.
    #[test]
    fn project_rejects_effective_cache_page_size_change() {
        let env = racer_io::Env::host();
        let target = new_target(&env);
        target.apply(publication(1)).unwrap();
        let small = PageSize::new(SMALL).unwrap();
        assert_eq!(
            target.project(sized(publication(2), Some(small))).err(),
            Some(Error::InvalidConfiguration)
        );
        // Spelling the inherited cluster default explicitly is not a change.
        let mut explicit = publication(2);
        let default = explicit.page_size;
        explicit.caches[0].page_size = Some(default);
        target.project(explicit).unwrap();

        let overridden = new_target(&env);
        overridden
            .apply(sized(publication(1), Some(small)))
            .unwrap();
        overridden
            .project(sized(publication(2), Some(small)))
            .unwrap();
        assert_eq!(
            overridden.project(publication(3)).err(),
            Some(Error::InvalidConfiguration)
        );
    }

    /// Acceptance repeats the check under the cell lock after a stale prepare.
    #[test]
    fn accept_rejects_effective_cache_page_size_change_under_lock() {
        let env = racer_io::Env::host();
        let target = new_target(&env);
        let small_size = PageSize::new(SMALL).unwrap();
        let small = target
            .project(sized(publication(1), Some(small_size)))
            .unwrap();
        let inherited = target.project(publication(2)).unwrap();
        target.accept(small, None).unwrap();
        assert_eq!(
            target.accept(inherited, None).err(),
            Some(Error::InvalidConfiguration)
        );
        assert_eq!(target.cursor().unwrap(), Some(PublicationSequence(1)));
        // The same effective size passes the repeated check.
        let same = target
            .project(sized(publication(2), Some(small_size)))
            .unwrap();
        target.accept(same, None).unwrap();
        assert_eq!(target.cursor().unwrap(), Some(PublicationSequence(2)));
    }

    /// A projection prepared before another acceptance cannot slip a different
    /// effective size past the commit-time check.
    #[test]
    fn stale_projection_cannot_change_effective_cache_page_size() {
        let env = racer_io::Env::host();
        let target = new_target(&env);
        let small = PageSize::new(SMALL).unwrap();
        let stale = target
            .projection(Arc::new(sized(publication(2), Some(small))))
            .unwrap();
        let accepted = target.projection(Arc::new(publication(1))).unwrap();
        target.accept(accepted().unwrap(), None).unwrap();
        assert_eq!(
            target.accept(stale().unwrap(), None).err(),
            Some(Error::InvalidConfiguration)
        );
        let current = target.current().unwrap();
        assert_eq!(current.sequence, PublicationSequence(1));
        assert_eq!(
            current.page_size_for(&CacheId(CACHE_A.into())),
            PageSize::DEFAULT
        );
    }

    /// New UIDs and removals are not size immutability: each may choose freely
    /// while caches that remain published keep their effective size.
    #[test]
    fn new_and_removed_cache_uids_may_use_any_page_size() {
        let env = racer_io::Env::host();
        let target = new_target(&env);
        target.apply(publication(1)).unwrap();
        let small = PageSize::new(SMALL).unwrap();
        let mut added = publication(2);
        add_cache(&mut added, CACHE_B, "cache-b", Some(small));
        let with_b = target.apply(added.clone()).unwrap();
        assert_eq!(with_b.page_size_for(&CacheId(CACHE_B.into())), small);

        let mut removed = added;
        removed.sequence.0 = 3;
        removed.membership_version.0 = 3;
        remove_cache(&mut removed, CACHE_A);
        let only_b = target.apply(removed.clone()).unwrap();
        assert_eq!(only_b.caches.len(), 1);
        assert_eq!(only_b.page_size_for(&CacheId(CACHE_B.into())), small);

        let mut added_c = removed;
        added_c.sequence.0 = 4;
        added_c.membership_version.0 = 4;
        add_cache(
            &mut added_c,
            CACHE_C,
            "cache-c",
            Some(PageSize::new(MEDIUM).unwrap()),
        );
        let with_c = target.apply(added_c).unwrap();
        assert_eq!(
            with_c.page_size_for(&CacheId(CACHE_C.into())),
            PageSize::new(MEDIUM).unwrap()
        );
        assert_eq!(with_c.page_size_for(&CacheId(CACHE_B.into())), small);
    }
}
