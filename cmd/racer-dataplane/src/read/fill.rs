//! Local disk -> ranked peer -> authorized origin acquisition and publication.
//!
//! Reserve progress memory/dirty capacity before download. Decrypt once per fill;
//! encrypt origin data once. Publish only verified whole pages, with original
//! ciphertext queued asynchronously on candidates. Disk failure may discard dirty
//! bytes. Origin 412 does not prove old copies absent from other permitted caches.
use super::{
    candidates::{CandidatePolicy, CandidateResolution},
    flight::{
        AcquisitionBudget, AcquisitionEvent, AcquisitionFailure, Flights, JoinedCopy, JoinedFlight,
    },
};
use crate::{
    error::{Error, Operation, Result},
    memory::{
        cache::MemoryCache,
        pool::{BufferPool, CiphertextPage},
    },
    model::{
        context::OriginContext,
        identity::PageId,
        metadata::{ObjectMetadata, VersionMetadata},
    },
    origin::client::Origin,
    peer::{
        requester::PeerClient,
        wire::{FetchMode, Operation as PeerOperation, PeerResponse},
    },
    runtime::{
        admission::{Admission, Reservation},
        deadline::RequestScope,
    },
    security::{aead::PageCrypto, credentials::CredentialCrypto},
    store::{reader::StoreReader, writer::StoreWriter},
    topology::membership::MembershipLease,
};
use std::{rc::Rc, sync::Arc};

pub use crate::memory::page::PageResult;
#[derive(Clone)]
pub struct FillDependencies {
    pub memory: Rc<MemoryCache>,
    pub buffers: Rc<BufferPool>,
    pub disk: Rc<StoreReader>,
    pub writer: Rc<StoreWriter>,
    pub peers: Rc<dyn PeerClient>,
    pub origin: Rc<dyn Origin>,
    pub candidates: Rc<CandidatePolicy>,
    pub flights: Rc<Flights>,
    pub crypto: Rc<PageCrypto>,
    pub credentials: Rc<CredentialCrypto>,
    pub admission: Rc<Admission>,
    /// Publish immutable descriptors to the page-zero owner over bounded commands.
    /// Page workers keep their own page-attached descriptor even if that catalog evicts it.
    pub metadata_owner: Arc<super::dispatch::WorkerDirectory>,
}
pub struct Fill {
    dependencies: FillDependencies,
}
impl Fill {
    pub fn new(dependencies: FillDependencies) -> Self {
        dependencies
            .candidates
            .set_credentials(dependencies.credentials.clone());
        Self { dependencies }
    }
    /// Local shard's memory/pending/disk descriptors, without starting acquisition.
    /// Used by WorkerDirectory's bounded retained-metadata lookup on a pinned miss.
    pub fn retained_metadata<'a>(
        &'a self,
        version: &'a crate::model::identity::ObjectVersion,
        scope: &'a RequestScope,
    ) -> Operation<'a, Option<crate::model::metadata::VersionMetadata>> {
        Box::pin(async move {
            scope.check()?;
            let mut found = None;
            for descriptor in [
                self.dependencies.memory.metadata(version)?,
                self.dependencies.writer.metadata(version)?,
                self.dependencies.disk.metadata(version)?,
            ]
            .into_iter()
            .flatten()
            {
                merge_metadata(&mut found, descriptor, version)?;
            }
            Ok(found)
        })
    }
    /// Join the shared flight with this request's borrowed context and original
    /// budget. Drive only an elected AcquisitionEvent::Lead; on retry re-resolve
    /// candidates using that waiter's membership, never a previous leader's grant.
    /// Publish the full PageResult after validation. Driver drop abandons leadership
    /// but worker-owned operations remain retained until actual completion fences.
    pub fn acquire<'a>(
        &'a self,
        page: PageId,
        membership: MembershipLease,
        context: &'a OriginContext,
        scope: &'a RequestScope,
        budget: &'a mut AcquisitionBudget,
    ) -> Operation<'a, PageResult> {
        self.acquire_with_prefetch(page, membership, context, scope, budget, None)
    }

    /// An initial GET discovers the identity before its body can enter a page
    /// flight. Join that exact flight; an existing leader wins and this redundant
    /// plaintext is dropped. Only the elected supplier encrypts/publishes it.
    pub fn publish_bootstrap_with_context<'a>(
        &'a self,
        origin: crate::origin::page::OriginPage,
        membership: MembershipLease,
        context: &'a OriginContext,
        scope: &'a RequestScope,
        budget: &'a mut AcquisitionBudget,
    ) -> Operation<'a, PageResult> {
        let page = PageId {
            version: origin.metadata.version.clone(),
            number: crate::model::identity::PageNumber(0),
        };
        self.acquire_with_prefetch(page, membership, context, scope, budget, Some(origin))
    }

    fn acquire_with_prefetch<'a>(
        &'a self,
        page: PageId,
        membership: MembershipLease,
        context: &'a OriginContext,
        scope: &'a RequestScope,
        budget: &'a mut AcquisitionBudget,
        mut prefetch: Option<crate::origin::page::OriginPage>,
    ) -> Operation<'a, PageResult> {
        Box::pin(async move {
            scope.check()?;
            if page.version.object != context.object {
                return Err(Error::InvalidRequest);
            }
            if let Some(result) = self.dependencies.memory.get(&page)? {
                result.validate_for(&page)?;
                return Ok(result);
            }
            let mut waiter = match self.dependencies.flights.join(
                page.clone(),
                membership,
                context,
                scope,
                budget,
            )? {
                JoinedFlight::Complete(result) => {
                    result.validate_for(&page)?;
                    return Ok(result);
                }
                JoinedFlight::Waiter(waiter) => waiter,
            };
            loop {
                match waiter.wait().await? {
                    AcquisitionEvent::Complete(result) => {
                        result.validate_for(&page)?;
                        return Ok(result);
                    }
                    AcquisitionEvent::Failed(error) => return Err(error),
                    AcquisitionEvent::Lead(leader) => {
                        let driver_permit = super::drivers::reserve()?;
                        let acquisition = waiter.acquisition(&leader)?;
                        // Seal/open creates an independently admitted operation owner;
                        // no raw secret clone or request borrow escapes into a worker job.
                        let mut nonce = [0; 16];
                        getrandom::getrandom(&mut nonce).map_err(|_| Error::Unavailable)?;
                        let attempt = crate::model::identity::AttemptId(nonce);
                        let sealed = self.dependencies.credentials.seal(
                            acquisition.origin,
                            attempt,
                            acquisition.scope,
                        )?;
                        let owned_context = self.dependencies.credentials.open_charged(
                            sealed,
                            acquisition.scope.request,
                            attempt,
                        )?;
                        let operation = self.dependencies.flights.retain_operation(&leader, ())?;
                        let mut owned_budget = acquisition.budget.transfer();
                        let owned_scope = acquisition.scope.clone();
                        let owned_membership = acquisition.membership.clone();
                        let owned_page = page.clone();
                        let prefetched = prefetch.take();
                        let fill = Fill::new(self.dependencies.clone());
                        let flights = self.dependencies.flights.clone();
                        let (send, mut receive) = futures::channel::oneshot::channel();
                        driver_permit.submit(Box::pin(async move {
                            let mut work = Box::pin(async {
                                if let Some(origin) = prefetched {
                                    fill.admit_bootstrap(
                                        origin,
                                        &owned_page,
                                        owned_membership,
                                        &owned_scope,
                                    )
                                    .await
                                } else {
                                    fill.acquire_once(
                                        &owned_page,
                                        owned_membership,
                                        &owned_context,
                                        &owned_scope,
                                        &mut owned_budget,
                                    )
                                    .await
                                }
                            });
                            let result = std::future::poll_fn(|cx| {
                                if operation.cancellation_requested() {
                                    let _ = owned_scope.cancel();
                                }
                                work.as_mut().poll(cx)
                            })
                            .await;
                            drop(work);
                            operation.complete()?;
                            match result {
                                Ok(result) => {
                                    let _ = flights.publish(leader, result);
                                }
                                Err(Error::OriginRejected) => {
                                    let _ =
                                        flights.fail(leader, AcquisitionFailure::OriginRejected);
                                }
                                Err(Error::OriginForbidden) => {
                                    let _ =
                                        flights.fail(leader, AcquisitionFailure::OriginForbidden);
                                }
                                Err(error) => {
                                    let _ =
                                        flights.fail(leader, AcquisitionFailure::Terminal(error));
                                }
                            }
                            let _ = send.send(owned_budget);
                            Ok(())
                        }));
                        use std::future::Future;
                        let remaining = std::future::poll_fn(|cx| {
                            super::drivers::poll(cx, 64);
                            acquisition.scope.cancellation.register(cx.waker())?;
                            acquisition.scope.check()?;
                            match std::pin::Pin::new(&mut receive).poll(cx) {
                                std::task::Poll::Ready(Ok(value)) => {
                                    std::task::Poll::Ready(Ok(value))
                                }
                                std::task::Poll::Ready(Err(_)) => {
                                    std::task::Poll::Ready(Err(Error::Unavailable))
                                }
                                std::task::Poll::Pending => std::task::Poll::Pending,
                            }
                        })
                        .await?;
                        *acquisition.budget = remaining;
                    }
                }
            }
        })
    }

    async fn admit_bootstrap(
        &self,
        origin: crate::origin::page::OriginPage,
        page: &PageId,
        membership: MembershipLease,
        scope: &RequestScope,
    ) -> Result<PageResult> {
        scope.check()?;
        if origin.metadata.version != page.version {
            return Err(Error::CorruptRecord);
        }
        use crate::{
            model::{limits::ResourceClass, range::PAGE_BYTES},
            runtime::reactor::IoBuffer,
        };
        if origin.plaintext.bytes()?.len()
            != origin.metadata.immutable().page_length(page)? as usize
        {
            return Err(Error::CorruptRecord);
        }
        let candidates = self
            .dependencies
            .candidates
            .candidates_async(membership, &page.version.object, page.number)
            .await?;
        if !self.dependencies.candidates.is_candidate(&candidates) {
            return Err(Error::Unauthorized);
        }
        let ciphertext = self.dependencies.admission.reserve(
            Some(&page.version.object.cache),
            ResourceClass::Ciphertext,
            PAGE_BYTES as usize + 16,
        )?;
        let dirty = self.dependencies.admission.reserve(
            Some(&page.version.object.cache),
            ResourceClass::DirtyCiphertext,
            PAGE_BYTES as usize + 16,
        )?;
        let (plaintext, ciphertext) = self
            .dependencies
            .crypto
            .encrypt(page.clone(), origin.plaintext, ciphertext, scope)
            .await?;
        let result = PageResult {
            metadata: origin.metadata,
            plaintext,
            ciphertext,
        };
        result.validate_for(page)?;
        self.publish(result.clone(), Some(dirty), scope).await?;
        Ok(result)
    }
    /// Strictly local completed/pending copy or join of existing work; never starts
    /// another acquisition or contacts origin. Use Flights::join_copy, whose waiter
    /// has no election/context API. Ciphertext preserves its nonce/tag.
    pub fn copy_only<'a>(
        &'a self,
        page: &'a PageId,
        scope: &'a RequestScope,
    ) -> Operation<'a, Option<(ObjectMetadata, CiphertextPage)>> {
        Box::pin(async move {
            scope.check()?;
            if let Some(copy) = self.dependencies.memory.ciphertext(page)? {
                validate_copy(&copy, page)?;
                return Ok(Some((copy.metadata, copy.ciphertext)));
            }
            if let Some(copy) = self.dependencies.writer.copy_only(page)? {
                validate_copy(&copy, page)?;
                return Ok(Some((copy.metadata, copy.ciphertext)));
            }
            match self.dependencies.disk.read(page, scope).await {
                Ok(Some(copy)) if validate_copy(&copy, page).is_ok() => {
                    return Ok(Some((copy.metadata, copy.ciphertext)));
                }
                Ok(_) | Err(Error::CorruptRecord | Error::MissingKey | Error::Io) => {}
                Err(error) => return Err(error),
            }
            match self.dependencies.flights.join_copy(page, scope)? {
                JoinedCopy::Miss => Ok(None),
                JoinedCopy::Complete(result) => {
                    result.validate_for(page)?;
                    Ok(Some((result.metadata, result.ciphertext)))
                }
                JoinedCopy::Waiter(mut waiter) => {
                    let result = waiter.wait().await?;
                    result.validate_for(page)?;
                    Ok(Some((result.metadata, result.ciphertext)))
                }
            }
        })
    }

    async fn acquire_once(
        &self,
        page: &PageId,
        membership: MembershipLease,
        context: &OriginContext,
        scope: &RequestScope,
        budget: &mut AcquisitionBudget,
    ) -> Result<PageResult> {
        scope.check()?;
        let candidates = self
            .dependencies
            .candidates
            .candidates_async(membership, &page.version.object, page.number)
            .await?;
        let persist = self.dependencies.candidates.is_candidate(&candidates);
        // Atomically reserve progress, including dirty capacity on candidates,
        // before touching transport. Retrying a bad local copy releases this batch.
        let mut reservation = self
            .dependencies
            .admission
            .reserve_fill(&context.object.cache, persist)?;
        let local = self.dependencies.writer.copy_only(page)?;
        let (local, token) = match local {
            Some(copy) => (Some(copy), None),
            None => match self.dependencies.disk.read_with_token(page, scope).await {
                Ok(Some((copy, token))) => (Some(copy), Some(token)),
                Ok(None) | Err(Error::CorruptRecord | Error::MissingKey | Error::Io) => {
                    (None, None)
                }
                Err(error) => return Err(error),
            },
        };
        if let Some(copy) = local {
            match self.decrypt(page, copy, reservation.plaintext, scope).await {
                Ok(result) => {
                    self.publish(result.clone(), None, scope).await?;
                    return Ok(result);
                }
                Err(Error::CorruptRecord | Error::MissingKey) => {
                    if let Some(token) = &token {
                        self.dependencies.disk.invalidate(token)?;
                    }
                    drop(reservation.ciphertext);
                    drop(reservation.dirty);
                    reservation = self
                        .dependencies
                        .admission
                        .reserve_fill(&context.object.cache, persist)?;
                }
                Err(error) => return Err(error),
            }
        }
        let operation = PeerOperation::Page {
            page: page.clone(),
            mode: FetchMode::Acquire,
        };
        // Keep the accepted ranking to probe later cached copies on an origin 412.
        let resolution = self
            .dependencies
            .candidates
            .resolve_with_budget(
                crate::topology::placement::Candidates {
                    membership: candidates.membership.clone(),
                    ordered: candidates.ordered.clone(),
                },
                context,
                operation,
                scope,
                budget,
            )
            .await?;
        let result = match resolution {
            CandidateResolution::Copy(response) => {
                let copy = response_copy(response.response(), page)?;
                self.decrypt(page, copy, reservation.plaintext, scope)
                    .await?
            }
            CandidateResolution::Origin(authority) => {
                authority.validate(&context.object, page.number)?;
                budget.begin_attempt(std::time::Instant::now(), scope.deadline.0)?;
                scope.check()?;
                match self
                    .dependencies
                    .origin
                    .page_reserved(&authority, context, page, reservation.plaintext, scope)
                    .await
                {
                    Ok(origin) => {
                        if origin.metadata.version != page.version {
                            return Err(Error::CorruptRecord);
                        }
                        let expected = origin.metadata.immutable().page_length(page)?;
                        use crate::runtime::reactor::IoBuffer;
                        if origin.plaintext.bytes()?.len() != expected as usize {
                            return Err(Error::CorruptRecord);
                        }
                        let (plaintext, ciphertext) = self
                            .dependencies
                            .crypto
                            .encrypt(
                                page.clone(),
                                origin.plaintext,
                                reservation.ciphertext,
                                scope,
                            )
                            .await?;
                        PageResult {
                            metadata: origin.metadata,
                            plaintext,
                            ciphertext,
                        }
                    }
                    Err(Error::VersionUnavailable) => {
                        let operation = PeerOperation::Page {
                            page: page.clone(),
                            mode: FetchMode::CopyOnly,
                        };
                        let response = self
                            .dependencies
                            .candidates
                            .remaining_copy(&candidates, context, &operation, scope, budget)
                            .await?
                            .ok_or(Error::VersionUnavailable)?;
                        self.decrypt(
                            page,
                            response_copy(response.response(), page)?,
                            self.dependencies.admission.reserve(
                                Some(&context.object.cache),
                                crate::model::limits::ResourceClass::Plaintext,
                                crate::model::range::PAGE_BYTES as usize,
                            )?,
                            scope,
                        )
                        .await?
                    }
                    Err(error) => return Err(error),
                }
            }
        };
        result.validate_for(page)?;
        scope.check()?;
        self.publish(result.clone(), reservation.dirty, scope)
            .await?;
        Ok(result)
    }

    async fn decrypt(
        &self,
        page: &PageId,
        copy: crate::memory::page::CiphertextCopy,
        reservation: Reservation,
        scope: &RequestScope,
    ) -> Result<PageResult> {
        validate_copy(&copy, page)?;
        // Keep the original immutable ciphertext lease across crypto submission.
        let plaintext = self
            .dependencies
            .crypto
            .decrypt(copy.ciphertext.clone(), reservation, scope)
            .await?;
        let result = PageResult {
            metadata: copy.metadata,
            plaintext,
            ciphertext: copy.ciphertext,
        };
        result.validate_for(page)?;
        Ok(result)
    }

    async fn publish(
        &self,
        result: PageResult,
        dirty: Option<Reservation>,
        scope: &RequestScope,
    ) -> Result<()> {
        result.validate_metadata()?;
        if let Some(existing) = self
            .retained_metadata(&result.metadata.version, scope)
            .await?
        {
            if existing.length != result.metadata.length {
                return Err(Error::CorruptRecord);
            }
        }
        self.dependencies.memory.publish(result.clone())?;
        // A descriptor catalog is an optimization. Every retained page owns its
        // immutable descriptor even when the catalog mailbox/capacity is saturated.
        match self
            .dependencies
            .metadata_owner
            .publish_metadata(result.metadata.immutable(), scope)
            .await
        {
            Ok(()) | Err(Error::Overloaded | Error::Unavailable) => {}
            Err(error) => return Err(error),
        }
        if let Some(dirty) = dirty {
            match self.dependencies.writer.enqueue(result.copy(), dirty) {
                Ok(_)
                | Err(Error::Overloaded | Error::Io | Error::Unavailable | Error::MissingKey) => {}
                Err(error) => return Err(error),
            }
        }
        Ok(())
    }
}
fn response_copy(
    response: &PeerResponse,
    page: &PageId,
) -> Result<crate::memory::page::CiphertextCopy> {
    match response {
        PeerResponse::Page {
            metadata,
            ciphertext,
        } => {
            let copy = crate::memory::page::CiphertextCopy {
                metadata: metadata.clone(),
                ciphertext: ciphertext.clone(),
            };
            validate_copy(&copy, page)?;
            Ok(copy)
        }
        _ => Err(Error::CorruptRecord),
    }
}
fn validate_copy(copy: &crate::memory::page::CiphertextCopy, page: &PageId) -> Result<()> {
    if &copy.ciphertext.envelope().page != page {
        return Err(Error::CorruptRecord);
    }
    copy.metadata
        .immutable()
        .validate_page(copy.ciphertext.envelope())
}
fn merge_metadata(
    found: &mut Option<VersionMetadata>,
    descriptor: VersionMetadata,
    version: &crate::model::identity::ObjectVersion,
) -> Result<()> {
    if &descriptor.version != version
        || found
            .as_ref()
            .is_some_and(|old| old.length != descriptor.length)
    {
        return Err(Error::CorruptRecord);
    }
    *found = Some(descriptor);
    Ok(())
}
#[cfg(test)]
#[path = "fill_tests.rs"]
mod integration_tests;
#[cfg(test)]
mod tests {
    use super::*;
    use crate::model::identity::{CacheId, CacheKey, ObjectId, ObjectVersion, StrongEtag};
    #[test]
    fn retained_metadata_never_substitutes_a_version_or_conflicting_length() {
        let version = ObjectVersion {
            object: ObjectId {
                cache: CacheId("cache".into()),
                key: CacheKey([0; 32]),
            },
            etag: StrongEtag::test_value("v1"),
        };
        let mut found = None;
        assert_eq!(
            merge_metadata(
                &mut found,
                VersionMetadata {
                    version: version.clone(),
                    length: 3
                },
                &version
            ),
            Ok(())
        );
        assert_eq!(
            merge_metadata(
                &mut found,
                VersionMetadata {
                    version: version.clone(),
                    length: 4
                },
                &version
            ),
            Err(Error::CorruptRecord)
        );
        assert_eq!(found.as_ref().unwrap().length, 3);
        let mut other = version.clone();
        other.etag = StrongEtag::test_value("v2");
        assert_eq!(
            merge_metadata(
                &mut found,
                VersionMetadata {
                    version: other,
                    length: 3
                },
                &version
            ),
            Err(Error::CorruptRecord)
        );
    }

    struct NeverPeer;
    impl PeerClient for NeverPeer {
        fn request<'a>(
            &'a self,
            _: crate::peer::wire::PeerRequest,
            _: &'a RequestScope,
        ) -> Operation<'a, crate::peer::wire::VerifiedResponse> {
            Box::pin(async { panic!("local owner must not contact a peer") })
        }
    }
    struct BytesOrigin {
        buffers: Rc<BufferPool>,
        calls: std::cell::Cell<usize>,
        reject: std::cell::Cell<bool>,
    }
    impl Origin for BytesOrigin {
        fn metadata<'a>(
            &'a self,
            _: &'a crate::read::candidates::OriginAuthority,
            _: &'a OriginContext,
            _: crate::model::metadata::MetadataSelector,
            _: &'a RequestScope,
        ) -> Operation<'a, crate::origin::metadata::MetadataReply> {
            Box::pin(async { panic!("pinned page acquisition does not refresh metadata") })
        }
        fn page<'a>(
            &'a self,
            _: &'a crate::read::candidates::OriginAuthority,
            _: &'a OriginContext,
            _: &'a PageId,
            _: &'a RequestScope,
        ) -> Operation<'a, crate::origin::page::OriginPage> {
            Box::pin(async { panic!("fill must transfer its progress reservation") })
        }
        fn page_reserved<'a>(
            &'a self,
            authority: &'a crate::read::candidates::OriginAuthority,
            context: &'a OriginContext,
            page: &'a PageId,
            reservation: Reservation,
            _: &'a RequestScope,
        ) -> Operation<'a, crate::origin::page::OriginPage> {
            Box::pin(async move {
                authority.validate(&context.object, page.number)?;
                self.calls.set(self.calls.get() + 1);
                if self.reject.replace(false) {
                    return Err(Error::OriginRejected);
                }
                let mut plaintext = self.buffers.plaintext(reservation, 3)?;
                use crate::runtime::reactor::IoBuffer;
                plaintext.bytes_mut()?.copy_from_slice(b"abc");
                Ok(crate::origin::page::OriginPage {
                    metadata: ObjectMetadata {
                        version: page.version.clone(),
                        length: 3,
                        expires_at: crate::model::metadata::ExpiresAt(std::time::UNIX_EPOCH),
                    },
                    plaintext,
                })
            })
        }
    }
    struct Rig {
        fill: Rc<Fill>,
        origin: Rc<BytesOrigin>,
        engine: crate::security::aead::PageCryptoEngine,
        client: Rc<crate::runtime::crypto::CryptoClient>,
        membership: MembershipLease,
        page: PageId,
    }
    fn rig() -> Rig {
        use crate::{
            model::identity::{MembershipVersion, PageNumber, WorkerId},
            runtime::{
                crypto,
                reactor::Reactor,
                worker::{CryptoRuntime, WorkerMap},
            },
            security::{
                aead::PageCryptoEngine,
                identity::tests::{CACHE, NODE},
            },
            store::{eviction::SegmentClock, index::Index, segment::Segments, slab::Slabs},
            topology::{
                membership::{Member, Membership},
                placement::Placement,
            },
        };
        let config = crate::test_support::cluster::config(false);
        let admission = Rc::new(Admission::new(config.limits.clone()));
        let reactor = Rc::new(Reactor::new(admission.clone()));
        let buffers = Rc::new(BufferPool::new(admission.clone()));
        let memory = Rc::new(MemoryCache::new(buffers.clone()));
        let index = Rc::new(Index::new(WorkerId(0), 16));
        let segments = Rc::new(Segments::new(WorkerId(0), 64 * 1024 * 1024));
        let slabs = Rc::new(Slabs::new(
            WorkerId(0),
            "unused".into(),
            reactor,
            1024 * 1024 * 1024,
            64 * 1024 * 1024,
        ));
        let eviction = Rc::new(SegmentClock::new(index.clone(), segments.clone(), 1));
        let disk = Rc::new(StoreReader::new(
            eviction,
            index.clone(),
            segments.clone(),
            slabs.clone(),
            buffers.clone(),
        ));
        let writer = Rc::new(StoreWriter::new(index, segments, slabs));
        let keys = Rc::new(crate::security::keyring::tests::keys());
        let (port, engine) = crypto::pair(WorkerId(0), 0, config.limits.queue_entries);
        let client = Rc::new(crypto::CryptoClient::new(port));
        let engine = PageCryptoEngine::new(CryptoRuntime { port: engine });
        let peers = Rc::new(NeverPeer);
        let candidates = Rc::new(CandidatePolicy::new(
            crate::model::identity::NodeId(NODE.into()),
            Rc::new(Placement::new(8)),
            peers.clone(),
        ));
        let credentials = Rc::new(CredentialCrypto::new(keys.clone(), admission.clone()));
        let origin = Rc::new(BytesOrigin {
            buffers: buffers.clone(),
            calls: std::cell::Cell::new(0),
            reject: std::cell::Cell::new(false),
        });
        let directory = Arc::new(
            crate::read::dispatch::WorkerDirectory::new(
                Arc::new(WorkerMap::new(vec![WorkerId(0)]).unwrap()),
                vec![WorkerId(0)],
                16,
            )
            .unwrap(),
        );
        let fill = Rc::new(Fill::new(FillDependencies {
            memory,
            buffers,
            disk,
            writer,
            peers,
            origin: origin.clone(),
            candidates,
            flights: Rc::new(Flights::new(admission.clone())),
            crypto: Rc::new(PageCrypto::new(keys, client.clone())),
            credentials,
            admission,
            metadata_owner: directory,
        }));
        let membership = Arc::new(
            Membership::validate(
                MembershipVersion(1),
                vec![Member {
                    node: crate::model::identity::NodeId(NODE.into()),
                    shares: std::num::NonZeroU32::new(4).unwrap(),
                    peer_endpoint: "127.0.0.1:8000".into(),
                    rails: vec![],
                    alignment_enabled: false,
                }],
            )
            .unwrap(),
        );
        let page = PageId {
            version: ObjectVersion {
                object: ObjectId {
                    cache: CacheId(CACHE.into()),
                    key: CacheKey([0; 32]),
                },
                etag: StrongEtag::parse(b"\"v1\"").unwrap(),
            },
            number: PageNumber(0),
        };
        Rig {
            fill,
            origin,
            engine,
            client,
            membership,
            page,
        }
    }
    fn drive<T>(
        engine: &mut crate::security::aead::PageCryptoEngine,
        client: &crate::runtime::crypto::CryptoClient,
        mut future: Operation<'_, T>,
    ) -> Result<T> {
        use crate::runtime::worker::CryptoService;
        let mut cx = std::task::Context::from_waker(futures::task::noop_waker_ref());
        for _ in 0..1000 {
            super::super::drivers::poll(&mut cx, 64);
            engine.poll_budgeted(64)?;
            client.poll_budgeted(64)?;
            if let std::task::Poll::Ready(result) = future.as_mut().poll(&mut cx) {
                return result;
            }
        }
        panic!("bounded local pipeline failed to make progress")
    }
    #[test]
    fn origin_fill_preserves_ciphertext_for_memory_and_pending_candidate_copy() {
        let mut rig = rig();
        let context = OriginContext {
            object: rig.page.version.object.clone(),
            metadata: None,
            authorization: None,
        };
        let scope = RequestScope::new(
            crate::model::identity::RequestId([1; 16]),
            std::time::Instant::now() + std::time::Duration::from_secs(30),
        )
        .unwrap();
        let mut budget = AcquisitionBudget::new(scope.deadline.0, 8, 8);
        let result = drive(
            &mut rig.engine,
            &rig.client,
            rig.fill.acquire(
                rig.page.clone(),
                rig.membership.clone(),
                &context,
                &scope,
                &mut budget,
            ),
        )
        .unwrap();
        assert_eq!(result.plaintext.bytes(), b"abc");
        assert_eq!(rig.origin.calls.get(), 1);
        assert_eq!(budget.remaining_attempts(), 7);
        let copy = futures::executor::block_on(rig.fill.copy_only(&rig.page, &scope))
            .unwrap()
            .unwrap();
        assert_eq!(copy.1.bytes(), result.ciphertext.bytes());
        assert_eq!(copy.1.envelope().nonce, result.ciphertext.envelope().nonce);
        let pending = rig
            .fill
            .dependencies
            .writer
            .copy_only(&rig.page)
            .unwrap()
            .unwrap();
        assert_eq!(pending.ciphertext.bytes(), result.ciphertext.bytes());
        let second = drive(
            &mut rig.engine,
            &rig.client,
            rig.fill.acquire(
                rig.page.clone(),
                rig.membership.clone(),
                &context,
                &scope,
                &mut budget,
            ),
        )
        .unwrap();
        assert_eq!(second.ciphertext.bytes(), result.ciphertext.bytes());
        assert_eq!(rig.origin.calls.get(), 1);
    }
}
