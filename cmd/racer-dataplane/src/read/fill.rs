//! Local disk -> ranked peer -> authorized origin acquisition and publication.
//!
//! Reserve progress memory/dirty capacity before download. Decrypt each copy once;
//! encrypt origin data once. Publish only verified whole pages, with original
//! ciphertext queued asynchronously on candidates. Disk failure may discard dirty
//! bytes. Origin 412 does not prove old copies absent from other permitted caches.

use super::candidates::CandidatePolicy;
use super::candidates::CandidateResolution;
use super::flight::AcquisitionBudget;
use super::flight::AcquisitionEvent;
use super::flight::AcquisitionFailure;
use super::flight::AcquisitionWaiter;
use super::flight::FlightLeader;
use super::flight::Flights;
use super::flight::JoinedCopy;
use super::flight::JoinedFlight;
use crate::admission::AdmissionPolicy;
use crate::admission::ResourceClass;
use crate::error::Error;
use crate::error::Operation;
use crate::error::Result;
use crate::memory::AcquiredPage;
use crate::memory::BufferPool;
use crate::memory::CiphertextPage;
use crate::memory::MemoryCache;
use crate::memory::PageResult;
use crate::memory::UnverifiedPage;
use crate::model::ObjectMetadata;
use crate::model::PAGE_BYTES;
use crate::model::PageId;
use crate::model::VersionMetadata;
use crate::origin::Origin;
use crate::peer::protocol::FetchMode;
use crate::peer::protocol::Operation as PeerOperation;
use crate::peer::protocol::PeerResponse;
use crate::runtime::RequestScope;
use crate::security::CredentialCrypto;
use crate::security::OriginContext;
use crate::security::PageCrypto;
use crate::store::StoreReader;
use crate::store::StoreWriter;
use crate::telemetry::Event;
use crate::telemetry::Gauge;
use crate::telemetry::LookupTier;
use crate::telemetry::Metrics;
use crate::telemetry::{Detail, FillAdmissionSite};
use std::rc::Rc;
use std::sync::Arc;
use uring_runtime::reactor::IoBuffer;

#[derive(Clone)]
pub struct FillDependencies {
    pub memory: Rc<MemoryCache>,
    pub buffers: BufferPool,
    pub disk: Rc<StoreReader>,
    pub writer: Rc<StoreWriter>,
    pub origin: Rc<dyn Origin>,
    pub candidates: Rc<CandidatePolicy>,
    pub flights: Rc<Flights>,
    pub crypto: Rc<PageCrypto>,
    pub credentials: Rc<CredentialCrypto>,
    pub admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
    /// Publish immutable descriptors to the page-zero owner over bounded commands.
    /// Page workers keep their own page-attached descriptor even if that catalog evicts it.
    pub metadata_owner: Arc<super::dispatch::WorkerDirectory>,
}
type LocalCopies = coalesce::shared::Table<PageId, Result<Option<UnverifiedPage>>>;
#[derive(Clone)]
pub struct Fill {
    pub(super) dependencies: FillDependencies,
    pub(super) metrics: Metrics,
    pub(super) local_copies: Rc<LocalCopies>,
}

/// Immediate acquisition tier, not the original producer or corruption location.
/// Retained includes flights, memory cache, and pending writer copies, even when
/// the retained copy still has a disk invalidation token.
#[derive(Clone, Copy)]
pub(super) enum DecryptSource {
    Disk,
    Retained,
    Peer,
}
impl DecryptSource {
    fn observe(self, metrics: &Metrics, result: Result<PageResult>) -> Result<PageResult> {
        result.inspect_err(|error| {
            if *error == Error::CorruptRecord {
                metrics.record(
                    match self {
                        Self::Disk => Event::FillDecryptDiskCorrupt,
                        Self::Retained => Event::FillDecryptRetainedCorrupt,
                        Self::Peer => Event::FillDecryptPeerCorrupt,
                    },
                    1,
                );
            }
        })
    }
}
impl Fill {
    #[cfg(test)]
    pub(crate) fn hedge_owner(&self) -> Option<&std::sync::Arc<super::candidates::Hedges>> {
        self.dependencies.candidates.hedge_owner()
    }
    pub(crate) fn cached_page(
        &self,
        page: &PageId,
        scope: &RequestScope,
    ) -> Result<Option<PageResult>> {
        scope.check()?;
        let result = self
            .metrics
            .lookup(LookupTier::Plaintext, self.dependencies.memory.get(page))?;
        if let Some(result) = &result {
            result.validate_for(page)?;
            self.metrics.record(Event::MemoryHit, 1);
        }
        Ok(result)
    }
    pub(crate) async fn select_subscription(
        &self,
        version: crate::model::ObjectVersion,
        demand: crate::peer::subscriptions::Demand,
        membership: std::sync::Arc<crate::topology::Membership>,
        context: &OriginContext,
        scope: &RequestScope,
        budget: &mut AcquisitionBudget,
    ) -> Result<PageResult> {
        let first = crate::model::PageNumber(
            demand
                .intervals()
                .first()
                .ok_or(Error::InvalidRequest)?
                .start,
        );
        let head = PageId {
            version: version.clone(),
            number: first,
        };
        if version.object != context.object {
            return Err(Error::InvalidRequest);
        }
        // A new or credit-starved subscriber can lag behind provider completion.
        // Reuse its verified local head before asking for another node transfer.
        // The stable page owner rechecks current cache/key admission.
        if let Some(result) = self
            .dependencies
            .metadata_owner
            .cached_page(head, scope)
            .await?
        {
            return Ok(result);
        }
        if let Some(response) = self
            .dependencies
            .candidates
            .subscribe(
                version.clone(),
                demand,
                &self.dependencies.metadata_owner.subscriptions,
                membership.clone(),
                context,
                scope,
                budget,
            )
            .await?
        {
            let PeerResponse::Selected { grant, .. } = response.response() else {
                return Err(Error::CorruptRecord);
            };
            let page = grant.page.clone();
            let accepted = async {
                let copy = response_copy(response.response(), &page)?;
                drop(response);
                // Allocate and authenticate on the stable owner, which can reclaim
                // every payload charge retained by its page cache.
                self.dependencies
                    .metadata_owner
                    .accept_selected(copy, scope)
                    .await
            }
            .await;
            match accepted {
                Ok(page) => return Ok(page),
                // A signed selection is not evidence of usable ciphertext. Fall
                // through once to ordinary ranked acquisition with the SAME
                // context and remaining budget; never recursively resubscribe.
                Err(Error::CorruptRecord | Error::MissingKey) => {}
                Err(error) => return Err(error),
            }
        }
        self.dependencies
            .metadata_owner
            .acquire(
                crate::model::PageId {
                    version,
                    number: first,
                },
                membership,
                context,
                scope,
                budget,
            )
            .await
    }

    pub(crate) async fn accept_selected(
        &self,
        mut copy: crate::memory::CiphertextCopy,
        scope: &RequestScope,
    ) -> Result<PageResult> {
        scope.check()?;
        copy.validate_metadata()?;
        let page = copy.ciphertext.envelope().page.clone();
        if !self
            .dependencies
            .admission
            .owns(&copy.ciphertext.inner.reservation)
        {
            let reservation = self.observe_reservation(
                scope,
                &page,
                FillAdmissionSite::SelectedCiphertext,
                || {
                    self.reserve_with_reclamation(
                        &page.version.object.cache,
                        ResourceClass::Ciphertext,
                        copy.ciphertext.inner.bytes.capacity(),
                    )
                },
            )?;
            copy.ciphertext = copy.ciphertext.rehome(reservation)?;
        }
        let reservation =
            self.observe_reservation(scope, &page, FillAdmissionSite::SelectedPlaintext, || {
                self.reserve_copy_plaintext(&page, &copy)
            })?;
        let result = self
            .decrypt(&page, copy, reservation, scope, DecryptSource::Peer)
            .await?;
        self.publish(result.clone(), None, scope).await?;
        // Selected subscription pages bypass acquire_inner's source accounting.
        // Count only authenticated, published reception on the stable owner.
        self.metrics.record(Event::PeerHit, 1);
        Ok(result)
    }
    pub(crate) fn observe_peer_error(
        &self,
        scope: &RequestScope,
        attempt: crate::model::AttemptId,
        error: Error,
    ) {
        use crate::telemetry::Failure;
        use crate::telemetry::Stage;
        self.dependencies.admission.policy().observer().record(
            Failure::new(Stage::PeerLocal, error)
                .request(scope)
                .attempt(attempt),
        );
    }
    pub(super) fn reserve_with_reclamation(
        &self,
        cache: &racer_control_wire::CacheId,
        class: ResourceClass,
        amount: usize,
    ) -> Result<flow_control::Charge<AdmissionPolicy>> {
        self.reserve_reclaiming(cache, class, amount, || {
            self.dependencies
                .admission
                .reserve(Some(cache), class, amount)
                .map_err(Into::into)
        })
    }

    pub(super) fn reserve_reclaiming(
        &self,
        cache: &racer_control_wire::CacheId,
        class: ResourceClass,
        amount: usize,
        reserve: impl Fn() -> Result<flow_control::Charge<AdmissionPolicy>>,
    ) -> Result<flow_control::Charge<AdmissionPolicy>> {
        let mut result = reserve();
        // At most two bounded scans, rechecking cache/global pressure each time.
        // Zero released bytes does not mean exhaustion: the memory cursor may
        // have crossed only busy or other-cache entries. No await allows new
        // local charges to interleave; remote completions can only free bytes.
        for _ in 0..2 {
            if !matches!(result, Err(Error::Overloaded)) {
                break;
            }
            let Some((owner, bytes)) = self
                .dependencies
                .admission
                .reclamation(cache, class, amount)
            else {
                break;
            };
            let released =
                self.dependencies
                    .memory
                    .reclaim_idle(class, owner.as_ref(), bytes, |page| {
                        self.dependencies.writer.discard_idle_copy(page)
                    });
            if released < bytes && matches!(class, ResourceClass::Ciphertext) {
                self.dependencies
                    .writer
                    .reclaim_ciphertext(owner.as_ref(), bytes - released);
            }
            result = reserve();
        }
        result
    }

    /// Observe mandatory acquisition only, never optional persistence or hedges.
    pub(super) async fn reserve_network_plaintext(
        &self,
        scope: &RequestScope,
        page: &PageId,
    ) -> Result<flow_control::Charge<AdmissionPolicy>> {
        self.reserve_network_plaintext_with(scope, page, || {
            self.dependencies
                .admission
                .reserve(
                    Some(&page.version.object.cache),
                    ResourceClass::Plaintext,
                    PAGE_BYTES as usize,
                )
                .map_err(Into::into)
        })
        .await
    }

    pub(super) async fn reserve_network_plaintext_with(
        &self,
        scope: &RequestScope,
        page: &PageId,
        mut reserve: impl FnMut() -> Result<flow_control::Charge<AdmissionPolicy>>,
    ) -> Result<flow_control::Charge<AdmissionPolicy>> {
        let admission = &self.dependencies.admission;
        let cache = &page.version.object.cache;
        let amount = PAGE_BYTES as usize;
        let mut cursor = self.dependencies.memory.plaintext_reclaim();
        let mut exhausted = false;
        loop {
            scope.check()?;
            let (mut result, mut detail) = admission.policy().capture_rejection(&mut reserve);
            if !matches!(result, Err(Error::Overloaded)) {
                return result;
            }
            let deficit = admission.reclamation(cache, ResourceClass::Plaintext, amount);
            if deficit.is_none() {
                // A remote completion may clear pressure after rejection. None
                // also means an impossible request, so retry exactly once here,
                // without restarting the scan or extending the caller's budget.
                scope.check()?;
                (result, detail) = admission.policy().capture_rejection(&mut reserve);
                if !matches!(result, Err(Error::Overloaded)) {
                    return result;
                }
            }
            if exhausted || deficit.is_none() {
                admission.policy().observer().final_fill_admission(
                    scope,
                    Some(page.number.0),
                    FillAdmissionSite::NetworkPlaintext,
                    detail,
                );
                return result;
            }
            let (owner, bytes) = deficit.unwrap();
            exhausted = self.dependencies.memory.reclaim_plaintext_quantum(
                &mut cursor,
                owner.as_ref(),
                bytes,
                |page| self.dependencies.writer.discard_idle_copy(page),
            );
            // Always yield between scan quanta and the next reservation, including
            // the final retry. Cancellation never extends the original allowance.
            crate::runtime::cooperative_turn().await;
        }
    }

    /// Observe mandatory acquisition only, never optional persistence or hedges.
    pub(super) fn observe_reservation<T>(
        &self,
        scope: &RequestScope,
        page: &PageId,
        site: FillAdmissionSite,
        operation: impl FnOnce() -> Result<T>,
    ) -> Result<T> {
        let (result, detail) = self
            .dependencies
            .admission
            .policy()
            .capture_rejection(operation);
        if matches!(result, Err(Error::Overloaded)) {
            self.dependencies
                .admission
                .policy()
                .observer()
                .final_fill_admission(scope, Some(page.number.0), site, detail);
        }
        result
    }

    /// StoreReader retries its callback after slab reclamation. Retain facts in
    /// this operation, never as an active policy capture across disk I/O.
    async fn read_disk_observed(
        &self,
        page: &PageId,
        scope: &RequestScope,
    ) -> Result<Option<(crate::memory::CiphertextCopy, crate::store::ReadToken)>> {
        self.read_disk_observed_with(page, scope, |amount| {
            self.reserve_with_reclamation(
                &page.version.object.cache,
                ResourceClass::Ciphertext,
                amount,
            )
        })
        .await
    }

    pub(super) async fn read_disk_observed_with(
        &self,
        page: &PageId,
        scope: &RequestScope,
        reserve: impl Fn(usize) -> Result<flow_control::Charge<AdmissionPolicy>>,
    ) -> Result<Option<(crate::memory::CiphertextCopy, crate::store::ReadToken)>> {
        let rejected = std::cell::Cell::new(None::<Detail>);
        let result = self
            .dependencies
            .disk
            .read_with_token_reclaim(page, scope, |amount| {
                let (result, detail) = self
                    .dependencies
                    .admission
                    .policy()
                    .capture_rejection(|| reserve(amount));
                rejected.set(detail);
                result
            })
            .await;
        if matches!(result, Err(Error::Overloaded)) && rejected.get().is_some() {
            self.dependencies
                .admission
                .policy()
                .observer()
                .final_fill_admission(
                    scope,
                    Some(page.number.0),
                    FillAdmissionSite::DiskCiphertext,
                    rejected.get(),
                );
        }
        result
    }

    /// Fresh metadata has no page identity yet. Admit its page-zero plaintext
    /// before origin I/O using the same bounded reclamation as ordinary fills.
    pub(crate) fn reserve_bootstrap(
        &self,
        cache: &racer_control_wire::CacheId,
    ) -> Result<flow_control::Charge<AdmissionPolicy>> {
        self.reserve_with_reclamation(cache, ResourceClass::Plaintext, PAGE_BYTES as usize)
    }

    /// A received or retained copy already fixes the output length. Validate it
    /// before reclaiming, without changing unknown-origin or hedge admission.
    pub(super) fn reserve_copy_plaintext(
        &self,
        page: &PageId,
        copy: &crate::memory::CiphertextCopy,
    ) -> Result<flow_control::Charge<AdmissionPolicy>> {
        validate_copy(copy, page)?;
        self.reserve_with_reclamation(
            &page.version.object.cache,
            ResourceClass::Plaintext,
            copy.ciphertext.envelope().plaintext_length as usize,
        )
    }
    pub fn new(dependencies: FillDependencies) -> Self {
        Self {
            dependencies,
            metrics: Metrics::default(),
            local_copies: Rc::new(LocalCopies::default()),
        }
    }
    pub fn with_metrics(mut self, metrics: Metrics) -> Self {
        self.metrics = metrics;
        self
    }
    /// Local shard's memory/pending/disk descriptors, without starting acquisition.
    /// Used by WorkerDirectory's bounded retained-metadata lookup on a pinned miss.
    pub fn retained_metadata<'a>(
        &'a self,
        version: &'a crate::model::ObjectVersion,
        scope: &'a RequestScope,
    ) -> Operation<'a, Option<crate::model::VersionMetadata>> {
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
        membership: std::sync::Arc<crate::topology::Membership>,
        context: &'a OriginContext,
        scope: &'a RequestScope,
        budget: &'a mut AcquisitionBudget,
    ) -> Operation<'a, PageResult> {
        Box::pin(async move {
            match self
                .acquire_with_prefetch(page, membership, context, scope, budget, None, true, None)
                .await?
            {
                AcquiredPage::Plaintext(page) => Ok(page),
                AcquiredPage::Ciphertext(_) => Err(Error::CorruptRecord),
            }
        })
    }

    pub(crate) async fn acquire_ordered(
        &self,
        page: PageId,
        membership: std::sync::Arc<crate::topology::Membership>,
        context: &OriginContext,
        scope: &RequestScope,
        budget: &mut AcquisitionBudget,
        guard: std::sync::Arc<super::range_stream::FixedAcquisition>,
    ) -> Result<PageResult> {
        match self
            .acquire_with_prefetch(
                page,
                membership,
                context,
                scope,
                budget,
                None,
                true,
                Some(guard),
            )
            .await?
        {
            AcquiredPage::Plaintext(page) => Ok(page),
            AcquiredPage::Ciphertext(_) => Err(Error::CorruptRecord),
        }
    }

    pub fn acquire_ciphertext<'a>(
        &'a self,
        page: PageId,
        membership: std::sync::Arc<crate::topology::Membership>,
        context: &'a OriginContext,
        scope: &'a RequestScope,
        budget: &'a mut AcquisitionBudget,
    ) -> Operation<'a, crate::memory::CiphertextCopy> {
        Box::pin(async move {
            Ok(self
                .acquire_with_prefetch(page, membership, context, scope, budget, None, false, None)
                .await?
                .copy())
        })
    }

    /// An initial GET discovers the identity before its body can enter a page
    /// flight. Join that exact flight; an existing leader wins and this redundant
    /// plaintext is dropped. Only the elected supplier encrypts/publishes it.
    pub fn publish_bootstrap_with_context<'a>(
        &'a self,
        origin: crate::origin::OriginPage,
        membership: std::sync::Arc<crate::topology::Membership>,
        context: &'a OriginContext,
        scope: &'a RequestScope,
        budget: &'a mut AcquisitionBudget,
    ) -> Operation<'a, PageResult> {
        let page = PageId {
            version: origin.metadata.version.clone(),
            number: crate::model::PageNumber(0),
        };
        Box::pin(async move {
            match self
                .acquire_with_prefetch(
                    page,
                    membership,
                    context,
                    scope,
                    budget,
                    Some(origin),
                    true,
                    None,
                )
                .await?
            {
                AcquiredPage::Plaintext(page) => Ok(page),
                AcquiredPage::Ciphertext(_) => Err(Error::CorruptRecord),
            }
        })
    }

    fn acquire_with_prefetch<'a>(
        &'a self,
        page: PageId,
        membership: std::sync::Arc<crate::topology::Membership>,
        context: &'a OriginContext,
        scope: &'a RequestScope,
        budget: &'a mut AcquisitionBudget,
        mut prefetch: Option<crate::origin::OriginPage>,
        plaintext: bool,
        guard: Option<std::sync::Arc<super::range_stream::FixedAcquisition>>,
    ) -> Operation<'a, AcquiredPage> {
        Box::pin(async move {
            scope.check()?;
            if page.version.object != context.object {
                return Err(Error::InvalidRequest);
            }
            if let Some(result) = self
                .metrics
                .lookup(LookupTier::Plaintext, self.dependencies.memory.get(&page))?
            {
                result.validate_for(&page)?;
                self.metrics.record(Event::MemoryHit, 1);
                return Ok(result.into());
            }
            let mut waiter = match self.dependencies.flights.join_for(
                page.clone(),
                membership,
                context,
                scope,
                budget,
                plaintext,
            )? {
                JoinedFlight::Complete(result) => {
                    // join checks current UID/key admission before sharing a
                    // completed bundle; registered waiters retain their own rights.
                    result.validate_for(&page)?;
                    return Ok(result.into());
                }
                JoinedFlight::Ciphertext(copy) => return Ok(AcquiredPage::Ciphertext(copy)),
                JoinedFlight::Waiter(waiter) => waiter,
            };
            loop {
                match waiter.wait().await? {
                    AcquisitionEvent::Complete(result) => {
                        result.validate_for(&page)?;
                        return Ok(result.into());
                    }
                    AcquisitionEvent::Ciphertext(copy) => {
                        return Ok(AcquiredPage::Ciphertext(copy));
                    }
                    AcquisitionEvent::Failed(error) => return Err(error),
                    AcquisitionEvent::Lead(leader) => {
                        self.drive_elected_acquisition(
                            &page,
                            leader,
                            &mut waiter,
                            &mut prefetch,
                            plaintext,
                            guard.clone(),
                        )
                        .await?;
                    }
                }
            }
        })
    }

    /// Transfer elected work to the worker driver. The caller only waits for its
    /// remaining budget; dropping that wait cannot release accepted work or the
    /// ordered-selection guard before the operation completes.
    async fn drive_elected_acquisition(
        &self,
        page: &PageId,
        leader: FlightLeader,
        waiter: &mut AcquisitionWaiter<'_>,
        prefetch: &mut Option<crate::origin::OriginPage>,
        plaintext: bool,
        completion_guard: Option<Arc<super::range_stream::FixedAcquisition>>,
    ) -> Result<()> {
        use std::future::Future;

        let ready = self.dependencies.flights.ciphertext_for(&leader)?;
        let ready = ready.or(self.metrics.lookup(
            LookupTier::Ciphertext,
            self.dependencies.memory.unverified(page),
        )?);
        let driver_permit = uring_runtime::drivers::reserve().map_err(Error::from)?;
        let acquisition = waiter.acquisition(&leader)?;
        // The driver stays on this owner. Admit an independent zeroizing context
        // without crossing an AEAD domain.
        let owned_context = self
            .dependencies
            .credentials
            .local_context(acquisition.origin, acquisition.scope)?;
        // Peer callers may share a worker-lifetime cancellation domain. Driver
        // abandonment must cancel only this work.
        let caller_scope = acquisition.scope.clone();
        let caller_cancellation = caller_scope.cancellation.subscribe()?;
        let owned_scope = RequestScope::new(caller_scope.request, caller_scope.deadline.0)?;
        let operation = self
            .dependencies
            .flights
            .retain_operation(&leader, self.metrics.lease(Gauge::ActiveFills)?)?;
        let mut owned_budget = acquisition.budget.transfer();
        let owned_membership = acquisition.membership.clone();
        let owned_page = page.clone();
        let prefetched = prefetch.take();
        let fill = self.clone();
        let flights = self.dependencies.flights.clone();
        let (send, mut receive) = futures::channel::oneshot::channel();
        driver_permit.submit_detached(Box::pin(async move {
            let _completion_guard = completion_guard;
            let mut work = Box::pin(async {
                if let Some(copy) = ready {
                    if !plaintext {
                        return Ok(AcquiredPage::Ciphertext(copy));
                    }
                    let result = async {
                        let reservation = fill.observe_reservation(
                            &owned_scope,
                            &owned_page,
                            FillAdmissionSite::RetainedPlaintext,
                            || fill.reserve_copy_plaintext(&owned_page, &copy.copy),
                        )?;
                        fill.decrypt(
                            &owned_page,
                            copy.copy.clone(),
                            reservation,
                            &owned_scope,
                            DecryptSource::Retained,
                        )
                        .await
                    }
                    .await;
                    match result {
                        Ok(result) => {
                            fill.publish(result.clone(), None, &owned_scope).await?;
                            return Ok(result.into());
                        }
                        Err(Error::CorruptRecord | Error::MissingKey) => {
                            fill.dependencies.memory.invalidate_ciphertext(&copy);
                            flights.discard_ciphertext(&leader)?;
                            if let Some(token) = copy.disk_token {
                                fill.dependencies.disk.invalidate(&token)?;
                            }
                        }
                        Err(error) => return Err(error),
                    }
                }
                if let Some(origin) = prefetched {
                    fill.admit_bootstrap(origin, &owned_page, owned_membership, &owned_scope)
                        .await
                        .map(AcquiredPage::from)
                } else {
                    fill.acquire_once(
                        &owned_page,
                        owned_membership,
                        &owned_context,
                        &owned_scope,
                        &mut owned_budget,
                        plaintext,
                    )
                    .await
                }
            });
            let result = std::future::poll_fn(|cx| {
                if !caller_scope.cancellation.is_cancelled() {
                    caller_cancellation.register(cx.waker());
                }
                if !owned_scope.cancellation.is_cancelled()
                    && (caller_scope.check().is_err() || operation.cancellation_requested())
                {
                    let _ = owned_scope.cancel();
                }
                work.as_mut().poll(cx)
            })
            .await;
            drop(work);
            operation.complete()?;
            match result {
                Ok(result) => {
                    if let AcquiredPage::Ciphertext(copy) = &result {
                        match fill.dependencies.memory.publish_ciphertext(copy.clone()) {
                            Ok(())
                            | Err(Error::Overloaded | Error::MissingKey | Error::Unavailable) => {}
                            Err(error) => return Err(error),
                        }
                    }
                    let _ = flights.publish_acquired(leader, result);
                }
                Err(Error::OriginRejected) => {
                    let _ = flights.fail(leader, AcquisitionFailure::OriginRejected);
                }
                Err(Error::OriginForbidden) => {
                    let _ = flights.fail(leader, AcquisitionFailure::OriginForbidden);
                }
                Err(error) => {
                    let _ = flights.fail(leader, AcquisitionFailure::Terminal(error));
                }
            }
            let _ = send.send(owned_budget);
            Ok(())
        }));
        let remaining = std::future::poll_fn(|cx| {
            uring_runtime::drivers::poll(cx, 64);
            acquisition.cancellation.register(cx.waker());
            acquisition.scope.check()?;
            std::pin::Pin::new(&mut receive)
                .poll(cx)
                .map(|result| result.map_err(|_| Error::Unavailable))
        })
        .await?;
        *acquisition.budget = remaining;
        Ok(())
    }

    async fn admit_bootstrap(
        &self,
        origin: crate::origin::OriginPage,
        page: &PageId,
        membership: std::sync::Arc<crate::topology::Membership>,
        scope: &RequestScope,
    ) -> Result<PageResult> {
        scope.check()?;
        if origin.metadata.version != page.version {
            return Err(Error::CorruptRecord);
        }
        use crate::admission::ResourceClass;
        use crate::model::PAGE_BYTES;
        if origin.plaintext.bytes()?.len()
            != origin.metadata.immutable().page_length(page)? as usize
        {
            return Err(Error::CorruptRecord);
        }
        let candidates = self
            .dependencies
            .candidates
            .candidates_scoped(membership, &page.version.object, page.number, scope)
            .await?;
        if !self.dependencies.candidates.is_candidate(&candidates) {
            return Err(Error::Unauthorized);
        }
        let ciphertext =
            self.observe_reservation(scope, page, FillAdmissionSite::OriginCiphertext, || {
                self.reserve_with_reclamation(
                    &page.version.object.cache,
                    ResourceClass::Ciphertext,
                    PAGE_BYTES as usize + 16,
                )
            })?;
        let dirty = match self.dependencies.admission.reserve(
            Some(&page.version.object.cache),
            ResourceClass::DirtyCiphertext,
            PAGE_BYTES as usize + 16,
        ) {
            Ok(reservation) => Some(reservation),
            Err(flow_control::Error::Overloaded) => None,
            Err(error) => return Err(error.into()),
        };
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
        self.publish(result.clone(), dirty, scope).await?;
        self.metrics.record(Event::OriginFill, 1);
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
            if let Some(copy) = self.metrics.lookup(
                LookupTier::Ciphertext,
                self.dependencies.memory.ciphertext(page),
            )? {
                validate_copy(&copy, page)?;
                self.metrics.record(Event::MemoryHit, 1);
                return Ok(Some((copy.metadata, copy.ciphertext)));
            }
            if let Some(copy) = self.metrics.lookup(
                LookupTier::Pending,
                self.dependencies.writer.copy_only(page),
            )? {
                validate_copy(&copy, page)?;
                self.metrics.record(Event::MemoryHit, 1);
                return Ok(Some((copy.metadata, copy.ciphertext)));
            }
            // Copy-only forbids new acquisition, not reclamation of idle local
            // buffers. Disk staging needs the same headroom as an acquire read.
            match self.local_disk_copy(page, scope).await {
                Ok(Some(UnverifiedPage { copy, .. })) => {
                    return Ok(Some((copy.metadata, copy.ciphertext)));
                }
                Err(Error::CorruptRecord) => {
                    self.metrics.record(Event::CorruptMiss, 1);
                }
                Ok(None) | Err(Error::MissingKey | Error::Io) => {}
                Err(error) => return Err(error),
            }
            match self.dependencies.flights.join_copy(page, scope)? {
                JoinedCopy::Ciphertext(copy) => {
                    Ok(Some((copy.copy.metadata, copy.copy.ciphertext)))
                }
                JoinedCopy::Miss => Ok(None),
                JoinedCopy::Complete(result) => {
                    result.validate_for(page)?;
                    Ok(Some((result.metadata, result.ciphertext)))
                }
                JoinedCopy::Waiter(mut waiter) => {
                    let result = waiter.wait().await?;
                    result.validate_for(page)?;
                    let copy = result.copy();
                    Ok(Some((copy.metadata, copy.ciphertext)))
                }
            }
        })
    }

    /// Coalesce only local disk work, with no origin context or election rights.
    /// The worker driver owns the entry and its charge until the disk completion
    /// fence, even if every caller detaches. Each caller owns a bounded waiter and
    /// cancellation subscription; one caller cannot cancel another caller's read.
    async fn local_disk_copy(
        &self,
        page: &PageId,
        scope: &RequestScope,
    ) -> Result<Option<UnverifiedPage>> {
        use std::future::Future;
        scope.check()?;
        let _waiter = self.dependencies.admission.reserve(
            Some(&page.version.object.cache),
            ResourceClass::Waiter,
            1,
        )?;
        let cancellation = scope.cancellation.subscribe()?;
        let existing = self.local_copies.get(page);
        let mut receive = if let Some(existing) = existing {
            existing
        } else {
            let permit = uring_runtime::drivers::reserve().map_err(Error::from)?;
            let flight = self.dependencies.admission.reserve(
                Some(&page.version.object.cache),
                ResourceClass::Flight,
                1,
            )?;
            let owned_scope = RequestScope::new(scope.request, scope.deadline.0)?;
            let (receive, completion) = self
                .local_copies
                .start(page.clone(), Err(Error::Unavailable));
            let fill = self.clone();
            let page = page.clone();
            permit.submit_detached(Box::pin(async move {
                let _flight = flight;
                let result = async {
                    let Some((copy, token)) = fill.read_disk_observed(&page, &owned_scope).await?
                    else {
                        return Ok(None);
                    };
                    fill.validate_disk_copy(&copy, &page, &token, &owned_scope)
                        .await?;
                    let copy = UnverifiedPage {
                        copy,
                        disk_token: Some(token),
                    };
                    match fill.dependencies.memory.publish_ciphertext(copy.clone()) {
                        Ok(())
                        | Err(Error::Overloaded | Error::MissingKey | Error::Unavailable) => {}
                        Err(error) => return Err(error),
                    }
                    fill.metrics.record(Event::DiskHit, 1);
                    Ok(Some(copy))
                }
                .await;
                completion.finish(result);
                Ok::<_, Error>(())
            }));
            receive
        };
        std::future::poll_fn(|cx| {
            cancellation.register(cx.waker());
            scope.check()?;
            uring_runtime::drivers::poll(cx, 64);
            std::pin::Pin::new(&mut receive).poll(cx)
        })
        .await
    }

    /// Read and validate local ciphertext before admitting its exact plaintext.
    /// A local miss leaves network acquisition's independent progress budget intact.
    pub(super) async fn acquire_local_copy(
        &self,
        page: &PageId,
        scope: &RequestScope,
        want_plaintext: bool,
    ) -> Result<Option<AcquiredPage>> {
        scope.check()?;
        // Pending copies already own ciphertext. Disk owns a separate complete
        // staging/decoded bundle; neither source needs speculative network bytes.
        let local = self.metrics.lookup(
            LookupTier::Pending,
            self.dependencies.writer.copy_only(page),
        )?;
        let (local, token) = match local {
            Some(copy) => (Some(copy), None),
            None => match self.read_disk_observed(page, scope).await {
                Ok(Some((copy, token))) => (Some(copy), Some(token)),
                Ok(None) | Err(Error::CorruptRecord | Error::MissingKey | Error::Io) => {
                    (None, None)
                }
                Err(error) => return Err(error),
            },
        };
        if let Some(copy) = local {
            if !want_plaintext {
                if let Some(token) = &token {
                    match self.validate_disk_copy(&copy, page, token, scope).await {
                        Ok(()) => {}
                        Err(Error::CorruptRecord) => {
                            self.metrics.record(Event::CorruptMiss, 1);
                            return Ok(None);
                        }
                        Err(Error::MissingKey) => return Ok(None),
                        Err(error) => return Err(error),
                    }
                }
            }
            if !want_plaintext && validate_copy(&copy, page).is_ok() {
                self.metrics.record(
                    if token.is_some() {
                        Event::DiskHit
                    } else {
                        Event::MemoryHit
                    },
                    1,
                );
                return Ok(Some(AcquiredPage::Ciphertext(UnverifiedPage {
                    copy,
                    disk_token: token,
                })));
            }
            let source = if token.is_some() {
                DecryptSource::Disk
            } else {
                DecryptSource::Retained
            };
            let result = async {
                scope.check()?;
                let reservation = self.observe_reservation(
                    scope,
                    page,
                    FillAdmissionSite::LocalPlaintext,
                    || self.reserve_copy_plaintext(page, &copy),
                )?;
                self.decrypt(page, copy, reservation, scope, source).await
            }
            .await;
            match result {
                Ok(result) => {
                    self.publish(result.clone(), None, scope).await?;
                    self.metrics.record(
                        if token.is_some() {
                            Event::DiskHit
                        } else {
                            Event::MemoryHit
                        },
                        1,
                    );
                    return Ok(Some(result.into()));
                }
                Err(error @ (Error::CorruptRecord | Error::MissingKey)) => {
                    if error == Error::CorruptRecord {
                        self.metrics.record(Event::CorruptMiss, 1);
                    }
                    if let Some(token) = &token {
                        self.dependencies.disk.invalidate(token)?;
                    }
                }
                Err(error) => return Err(error),
            }
        }
        Ok(None)
    }

    pub(super) async fn validate_disk_copy(
        &self,
        copy: &crate::memory::CiphertextCopy,
        page: &PageId,
        token: &crate::store::ReadToken,
        scope: &RequestScope,
    ) -> Result<()> {
        let result = match validate_copy(copy, page) {
            Ok(()) => {
                self.dependencies
                    .crypto
                    .verify_checksum(copy.ciphertext.clone(), scope)
                    .await
            }
            Err(error) => Err(error),
        };
        if matches!(result, Err(Error::CorruptRecord | Error::MissingKey)) {
            self.dependencies.disk.invalidate(token)?;
        }
        result
    }

    pub(super) async fn acquire_once(
        &self,
        page: &PageId,
        membership: std::sync::Arc<crate::topology::Membership>,
        context: &OriginContext,
        scope: &RequestScope,
        budget: &mut AcquisitionBudget,
        want_plaintext: bool,
    ) -> Result<AcquiredPage> {
        if let Some(result) = self.acquire_local_copy(page, scope, want_plaintext).await? {
            return Ok(result);
        }
        let candidates = self
            .dependencies
            .candidates
            .candidates_scoped(membership, &page.version.object, page.number, scope)
            .await?;
        let persist = self.dependencies.candidates.is_candidate(&candidates);
        // Cipher-only acquisitions reserve no plaintext until origin actually
        // supplies a page. Requester consumers still authenticate before acceptance.
        let mut plaintext = if want_plaintext {
            Some(self.reserve_network_plaintext(scope, page).await?)
        } else {
            None
        };
        let dirty = if persist {
            match self.dependencies.admission.reserve(
                Some(&context.object.cache),
                ResourceClass::DirtyCiphertext,
                PAGE_BYTES as usize + 16,
            ) {
                Ok(reservation) => Some(reservation),
                Err(flow_control::Error::Overloaded) => None,
                Err(error) => return Err(error.into()),
            }
        } else {
            None
        };
        let operation = PeerOperation::Page {
            page: page.clone(),
            mode: FetchMode::Acquire,
        };
        let mut hedge_continuation = super::candidates::HedgeContinuation::default();
        if want_plaintext {
            if let Some(result) = self
                .dependencies
                .candidates
                .hedge_page(
                    &candidates,
                    context,
                    &operation,
                    scope,
                    budget,
                    &self.dependencies.admission,
                    &mut hedge_continuation,
                    |response, child| {
                        Box::pin(async move {
                            let copy = response_copy(response.response(), page)?;
                            if let Some(existing) =
                                self.retained_metadata(&page.version, &child).await?
                            {
                                if !existing.compatible(&copy.metadata.immutable()) {
                                    return Err(Error::CorruptRecord);
                                }
                            }
                            let result =
                                self.decrypt_response(page, response, None, &child).await?;
                            // A sibling may publish while crypto runs. Recheck before
                            // election, not only after a winner has canceled its peer.
                            if let Some(existing) =
                                self.retained_metadata(&page.version, &child).await?
                            {
                                if !existing.compatible(&result.metadata.immutable()) {
                                    return Err(Error::CorruptRecord);
                                }
                            }
                            Ok(result)
                        })
                    },
                )
                .await?
            {
                result.validate_for(page)?;
                scope.check()?;
                self.publish(result.clone(), dirty, scope).await?;
                self.metrics.record(Event::PeerHit, 1);
                return Ok(result.into());
            }
        }
        // Keep the accepted ranking to probe later cached copies on an origin 412.
        let resolution = self
            .dependencies
            .candidates
            .resolve_after_hedge(
                crate::topology::Candidates {
                    membership: candidates.membership.clone(),
                    ordered: candidates.ordered.clone(),
                },
                context,
                operation,
                scope,
                budget,
                |response| {
                    let reserved = plaintext.take();
                    Box::pin(async move {
                        if want_plaintext {
                            self.decrypt_response(page, response, reserved, scope)
                                .await
                                .map(AcquiredPage::from)
                        } else {
                            Ok(AcquiredPage::Ciphertext(UnverifiedPage {
                                copy: response_copy(response.response(), page)?,
                                disk_token: None,
                            }))
                        }
                    })
                },
                hedge_continuation,
            )
            .await?;
        let mut source = Event::PeerHit;
        let result = match resolution {
            CandidateResolution::Copy(result) => result,
            CandidateResolution::Origin(authority) => {
                authority.validate(&context.object, page.number)?;
                budget.begin_attempt(uring_runtime::environment::now(), scope.deadline.0)?;
                scope.check()?;
                // Peer reception owns its ciphertext allocation. Reserve encryption
                // output only when this candidate actually needs an origin fill.
                let ciphertext = self.observe_reservation(
                    scope,
                    page,
                    FillAdmissionSite::OriginCiphertext,
                    || {
                        self.reserve_with_reclamation(
                            &context.object.cache,
                            ResourceClass::Ciphertext,
                            PAGE_BYTES as usize + 16,
                        )
                    },
                )?;
                let plaintext = match plaintext.take() {
                    Some(reserved) => reserved,
                    None => self.observe_reservation(
                        scope,
                        page,
                        FillAdmissionSite::OriginPlaintext,
                        || self.reserve_bootstrap(&context.object.cache),
                    )?,
                };
                match self
                    .dependencies
                    .origin
                    .page_reserved(&authority, context, page, plaintext, scope)
                    .await
                {
                    Ok(origin) => {
                        source = Event::OriginFill;
                        self.encrypt_origin_page(page, origin, ciphertext, scope)
                            .await?
                            .into()
                    }
                    Err(Error::VersionUnavailable) => {
                        let operation = PeerOperation::Page {
                            page: page.clone(),
                            mode: FetchMode::CopyOnly,
                        };
                        self.dependencies
                            .candidates
                            .remaining_validated_copy(
                                &candidates,
                                context,
                                &operation,
                                scope,
                                budget,
                                |response| {
                                    Box::pin(async move {
                                        if want_plaintext {
                                            self.decrypt_response(page, response, None, scope)
                                                .await
                                                .map(AcquiredPage::from)
                                        } else {
                                            Ok(AcquiredPage::Ciphertext(UnverifiedPage {
                                                copy: response_copy(response.response(), page)?,
                                                disk_token: None,
                                            }))
                                        }
                                    })
                                },
                            )
                            .await?
                            .ok_or_else(|| {
                                self.dependencies
                                    .candidates
                                    .final_origin_miss(&authority, &operation, scope, budget)
                            })?
                    }
                    Err(error) => return Err(error),
                }
            }
        };
        result.validate_for(page)?;
        scope.check()?;
        if let AcquiredPage::Plaintext(page) = &result {
            // Origin encryption already produced a verified whole page. Preserve
            // that evidence even when the supplier requested only ciphertext.
            self.publish(page.clone(), dirty, scope).await?;
        }
        self.metrics.record(source, 1);
        Ok(result)
    }

    async fn encrypt_origin_page(
        &self,
        page: &PageId,
        origin: crate::origin::OriginPage,
        ciphertext: flow_control::Charge<AdmissionPolicy>,
        scope: &RequestScope,
    ) -> Result<PageResult> {
        use uring_runtime::reactor::IoBuffer;
        if origin.metadata.version != page.version {
            return Err(Error::CorruptRecord);
        }
        let expected = origin.metadata.immutable().page_length(page)?;
        if origin.plaintext.bytes()?.len() != expected as usize {
            return Err(Error::CorruptRecord);
        }
        let (plaintext, ciphertext) = self
            .dependencies
            .crypto
            .encrypt(page.clone(), origin.plaintext, ciphertext, scope)
            .await?;
        Ok(PageResult {
            metadata: origin.metadata,
            plaintext,
            ciphertext,
        })
    }

    pub(super) async fn decrypt_response(
        &self,
        page: &PageId,
        response: crate::peer::forwarding::VerifiedResponse,
        reservation: Option<flow_control::Charge<AdmissionPolicy>>,
        scope: &RequestScope,
    ) -> Result<PageResult> {
        scope.check()?;
        let copy = response_copy(response.response(), page).inspect_err(|error| {
            if *error == Error::CorruptRecord {
                self.metrics.record(Event::CorruptMiss, 1);
            }
        })?;
        let reservation = match reservation {
            Some(reserved) => reserved,
            None => {
                self.observe_reservation(scope, page, FillAdmissionSite::ResponsePlaintext, || {
                    self.reserve_copy_plaintext(page, &copy)
                })?
            }
        };
        self.decrypt(page, copy, reservation, scope, DecryptSource::Peer)
            .await
            .inspect_err(|error| {
                if *error == Error::CorruptRecord {
                    self.metrics.record(Event::CorruptMiss, 1);
                }
            })
    }

    pub(super) async fn decrypt(
        &self,
        page: &PageId,
        copy: crate::memory::CiphertextCopy,
        reservation: flow_control::Charge<AdmissionPolicy>,
        scope: &RequestScope,
        source: DecryptSource,
    ) -> Result<PageResult> {
        // These source counters include structural CorruptRecord rejections in
        // this helper. They are independent of the exact CRC/AEAD engine counters,
        // which are reaped even if a waiting fill has been abandoned.
        let result = async {
            validate_copy(&copy, page)?;
            // Keep the original immutable ciphertext lease across crypto submission.
            self.metrics.record(Event::PageDecrypt, 1);
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
        .await;
        source.observe(&self.metrics, result)
    }

    pub(super) async fn publish(
        &self,
        result: PageResult,
        dirty: Option<flow_control::Charge<AdmissionPolicy>>,
        scope: &RequestScope,
    ) -> Result<()> {
        result.validate_metadata()?;
        if let Some(existing) = self
            .retained_metadata(&result.metadata.version, scope)
            .await?
        {
            if !existing.compatible(&result.metadata.immutable()) {
                return Err(Error::CorruptRecord);
            }
        }
        match self.dependencies.memory.publish(result.clone()) {
            Ok(()) | Err(Error::Overloaded | Error::Unavailable | Error::MissingKey) => {}
            Err(error) => return Err(error),
        }
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
            match self
                .dependencies
                .writer
                .enqueue_reclaiming(result.copy(), dirty, |amount| {
                    let cache = &result.metadata.version.object.cache;
                    self.reserve_reclaiming(cache, ResourceClass::Ciphertext, amount, || {
                        self.dependencies
                            .admission
                            .reserve_completion(Some(cache), ResourceClass::Ciphertext, amount)
                            .map_err(Into::into)
                    })
                }) {
                Ok(_)
                | Err(Error::Overloaded | Error::Io | Error::Unavailable | Error::MissingKey) => {}
                Err(error) => return Err(error),
            }
        }
        Ok(())
    }
}
fn response_copy(response: &PeerResponse, page: &PageId) -> Result<crate::memory::CiphertextCopy> {
    match response {
        PeerResponse::Page {
            metadata,
            ciphertext,
        }
        | PeerResponse::Selected {
            metadata,
            ciphertext,
            ..
        } => {
            let copy = crate::memory::CiphertextCopy {
                metadata: metadata.clone(),
                ciphertext: ciphertext.clone(),
            };
            validate_copy(&copy, page)?;
            Ok(copy)
        }
        _ => Err(Error::CorruptRecord),
    }
}
pub(super) fn validate_copy(copy: &crate::memory::CiphertextCopy, page: &PageId) -> Result<()> {
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
    version: &crate::model::ObjectVersion,
) -> Result<()> {
    if &descriptor.version != version
        || found
            .as_ref()
            .is_some_and(|old| !old.compatible(&descriptor))
    {
        return Err(Error::CorruptRecord);
    }
    *found = Some(descriptor);
    Ok(())
}
#[cfg(test)]
mod tests {
    use super::*;
    use crate::model::CacheKey;
    use crate::model::ObjectId;
    use crate::model::ObjectVersion;
    use crate::model::StrongEtag;
    use racer_control_wire::CacheId;
    #[test]
    fn decrypt_source_counters_preserve_results_and_ignore_non_corruption() {
        let metrics = Metrics::default();
        for (source, event) in [
            (DecryptSource::Disk, Event::FillDecryptDiskCorrupt),
            (DecryptSource::Retained, Event::FillDecryptRetainedCorrupt),
            (DecryptSource::Peer, Event::FillDecryptPeerCorrupt),
        ] {
            assert_eq!(
                source
                    .observe(&metrics, Ok(crate::read::tests::page(42)))
                    .unwrap()
                    .plaintext
                    .bytes(),
                &[42]
            );
            for error in [
                Error::Cancelled,
                Error::MissingKey,
                Error::Overloaded,
                Error::Io,
            ] {
                assert_eq!(source.observe(&metrics, Err(error)).err(), Some(error));
            }
            assert_eq!(metrics.count(event), 0);
            assert_eq!(
                source.observe(&metrics, Err(Error::CorruptRecord)).err(),
                Some(Error::CorruptRecord)
            );
            assert_eq!(metrics.count(event), 1);
        }
    }
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
                    content_type: None,
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
                    content_type: None,
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
                    content_type: None,
                    version: other,
                    length: 3
                },
                &version
            ),
            Err(Error::CorruptRecord)
        );
    }
}
