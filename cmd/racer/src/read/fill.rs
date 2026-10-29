//! Local disk -> ranked peer -> authorized origin acquisition and publication.
//!
//! Reserve mandatory progress memory before download. Decrypt each copy once;
//! encrypt origin data once. Publish only verified whole pages, with original
//! ciphertext queued best effort under second-sight policy. Disk failure may discard dirty
//! bytes. Origin 412 does not prove old copies absent from other permitted caches.

use super::candidates::CandidatePolicy;
use super::flight::AcquisitionBudget;
use super::flight::AcquisitionEvent;
use super::flight::AcquisitionFailure;
use super::flight::AcquisitionWaiter;
use super::flight::FlightLeader;
use super::flight::Flights;
use super::flight::JoinedCopy;
use super::flight::JoinedFlight;
use super::flight::{DriverDiagnostic, DriverStage};
use super::merge_metadata;
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
type LocalCopies = flow_control::coalesce::shared::Table<PageId, Result<Option<UnverifiedPage>>>;
/// Local plaintext recovery keyed by page: one decrypt of a retained or local
/// copy is shared by every concurrent warm reader and by elected acquisitions
/// that find the page cached. Local-only readers never start acquisition.
type LocalPlaintext = flow_control::coalesce::shared::Table<PageId, Result<Option<PageResult>>>;
/// Source key of each running local plaintext job, once its copy is chosen.
/// New followers are admitted only while that key remains available.
type LocalPlaintextKeys = std::cell::RefCell<
    std::collections::BTreeMap<PageId, Rc<std::cell::Cell<Option<racer_control_wire::KeyId>>>>,
>;

/// A host descriptor to hand to cuFile, or None for simulated descriptors.
fn host_descriptor(
    descriptor: &uring_runtime::reactor::descriptor::Descriptor,
) -> Option<std::os::fd::OwnedFd> {
    use std::os::fd::AsFd;
    #[cfg(test)]
    if descriptor.as_sim().is_some() {
        return None;
    }
    descriptor.as_fd().try_clone_to_owned().ok()
}

use racer_core::read::LeaderSource;
pub(super) use racer_core::read::optional_headroom;
#[derive(Clone)]
pub struct Fill {
    pub(super) dependencies: FillDependencies,
    pub(super) metrics: Metrics,
    pub(super) local_copies: Rc<LocalCopies>,
    pub(super) local_plaintext: Rc<LocalPlaintext>,
    local_plaintext_keys: Rc<LocalPlaintextKeys>,

    diagnostic: Option<DriverDiagnostic>,
    fence_acquisition: bool,
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
/// Duration metrics for serial driver calls. Candidate calls are timed at their call sites.
fn stage_events(stage: DriverStage) -> Option<(Event, Event)> {
    match stage {
        DriverStage::PlaintextReclaim => Some((
            Event::FillPlaintextReclaimCount,
            Event::FillPlaintextReclaimNs,
        )),
        DriverStage::PublishCall => Some((Event::FillPublishCount, Event::FillPublishNs)),
        DriverStage::DecryptCall => Some((Event::FillDecryptCount, Event::FillDecryptNs)),
        DriverStage::DiskReadCall => Some((Event::FillDiskReadCount, Event::FillDiskReadNs)),
        DriverStage::OriginCall => Some((Event::FillOriginCount, Event::FillOriginNs)),
        DriverStage::EncryptCall => Some((Event::FillEncryptCount, Event::FillEncryptNs)),
        DriverStage::ChecksumCall => Some((Event::FillChecksumCount, Event::FillChecksumNs)),
        DriverStage::Starting
        | DriverStage::BetweenCalls
        | DriverStage::CandidateCompositeCall
        | DriverStage::WorkReturned => None,
    }
}
/// Bound on retries after a joined local plaintext job fails under its
/// starter's inherited scope.
const LOCAL_PLAINTEXT_JOIN_RETRIES: usize = 2;

/// Providers a `gpu-read` asks, in visit order, to land a page in GPU
/// memory before it takes the usual path. Each is one CopyOnly request
/// against the read's acquisition budget; more would add a round trip per
/// miss before the usual path asks the same providers again.
const LANDING_PROVIDERS: usize = 2;

impl Fill {
    /// Published mode of `cache`.
    pub(crate) fn mode(
        &self,
        cache: &racer_control_wire::CacheId,
    ) -> racer_control_wire::CacheMode {
        self.dependencies.memory.availability().mode(cache)
    }

    /// Whether `cache` retains no pages or metadata on this node.
    pub(crate) fn passthrough(&self, cache: &racer_control_wire::CacheId) -> bool {
        self.mode(cache).passthrough()
    }

    /// The reader's retention decision for `page`: second-sight history when
    /// `observe`, else ownership only. Passthrough caches never persist and do
    /// not record history.
    fn retention_observation(
        &self,
        page: &PageId,
        interest: &crate::retention::Interest,
        observe: bool,
    ) -> crate::retention::Observation {
        let retention = self.dependencies.writer.retention();
        if self.passthrough(&page.version.object.cache) {
            crate::retention::Observation::never()
        } else if observe {
            retention.observe(page, interest)
        } else {
            retention.owned_only(page)
        }
    }

    pub(super) async fn warm_local(
        &self,
        page: &PageId,
        scope: &RequestScope,
    ) -> Result<PageResult> {
        if let Some(result) = self.cached_page(page, scope)? {
            return Ok(result);
        }
        match self.local_plaintext_copy(page, scope, true).await? {
            Some(result) => Ok(result),
            None => Err(Error::Unavailable),
        }
    }

    /// Coalesce local decrypts so concurrent warm readers and elected
    /// acquisitions reserve and decrypt one plaintext. A detached worker driver
    /// owns the shared work, so one caller's cancellation never fails another
    /// caller. The driver only reads pending, cached, or disk copies; it never
    /// contacts peers or origin, so a local-only caller cannot start acquisition.
    ///
    /// A `probe` caller owns its own Waiter charge, and a probe that starts the
    /// job reserves a Flight charge retained by the detached driver until the
    /// job completes. An elected acquisition already holds both through its
    /// flight registration and awaits the job inside its retained operation,
    /// staying attached through cancellation until the job completes.
    /// New followers recheck current cache and source-key admission; callers
    /// admitted earlier keep their completion rights.
    async fn local_plaintext_copy(
        &self,
        page: &PageId,
        scope: &RequestScope,
        probe: bool,
    ) -> Result<Option<PageResult>> {
        use std::future::Future;
        scope.check()?;
        let _waiter = if probe {
            Some(self.dependencies.admission.reserve(
                Some(&page.version.object.cache),
                ResourceClass::Waiter,
                1,
            )?)
        } else {
            None
        };
        let cancellation = scope.cancellation.subscribe()?;
        // A joined job runs under the scope of the caller that started it. If
        // that inherited scope expires or is cancelled while this caller's own
        // scope is still valid, retry under this caller's own scope. A job this
        // caller started shares its deadline, so its failure is final. The
        // bound stops a stream of short-lived starters from looping forever.
        let mut joins_left = LOCAL_PLAINTEXT_JOIN_RETRIES;
        loop {
            let mut started = false;
            let mut receive = if let Some(existing) = self.local_plaintext.get(page) {
                let key = self
                    .local_plaintext_keys
                    .borrow()
                    .get(page)
                    .and_then(|key| key.get());
                self.dependencies.flights.admit_local(page, key)?;
                existing
            } else {
                self.dependencies.flights.admit_local(page, None)?;
                started = true;
                let permit = uring_runtime::drivers::reserve().map_err(Error::from)?;
                let flight = if probe {
                    Some(self.dependencies.flights.reserve_copy_flight(page, scope)?)
                } else {
                    None
                };
                let owned_scope = RequestScope::new(scope.request, scope.deadline.0)?;
                let (receive, completion) = self
                    .local_plaintext
                    .start(page.clone(), Err(Error::Unavailable));
                if let Some(completion) = completion {
                    let source = Rc::new(std::cell::Cell::new(None));
                    self.local_plaintext_keys
                        .borrow_mut()
                        .insert(page.clone(), source.clone());
                    let mut fill = self.clone();
                    fill.diagnostic = None;
                    let page = page.clone();
                    permit.submit_detached(Box::pin(async move {
                        let _flight = flight;
                        let result = match fill.cached_page(&page, &owned_scope) {
                            Ok(Some(result)) => Ok(Some(result)),
                            Ok(None) => {
                                match fill.retained_plaintext(&page, &owned_scope, &source).await {
                                    Ok(Some(result)) => Ok(Some(result)),
                                    Ok(None) => fill
                                        .acquire_local_copy_from(
                                            &page,
                                            &owned_scope,
                                            true,
                                            Some(&source),
                                        )
                                        .await
                                        .map(|acquired| match acquired {
                                            Some(AcquiredPage::Plaintext(result)) => Some(result),
                                            _ => None,
                                        }),
                                    Err(error) => Err(error),
                                }
                            }
                            Err(error) => Err(error),
                        };
                        let mut keys = fill.local_plaintext_keys.borrow_mut();
                        if keys.get(&page).is_some_and(|key| Rc::ptr_eq(key, &source)) {
                            keys.remove(&page);
                        }
                        drop(keys);
                        completion.finish(result);
                        Ok::<_, Error>(())
                    }));
                }
                receive
            };
            let result = std::future::poll_fn(|cx| {
                cancellation.register(cx.waker());
                // A probe-led job owns its own Flight charge, so a probe may
                // detach. An acquisition-led job has no charge of its own and
                // relies on the acquisition's retained operation, so acquisition
                // callers stay attached until the job completes and only then
                // report cancellation or deadline expiry.
                if probe {
                    scope.check()?;
                }
                uring_runtime::drivers::poll(cx, 64);
                let result = std::task::ready!(std::pin::Pin::new(&mut receive).poll(cx));
                scope.check()?;
                std::task::Poll::Ready(result)
            })
            .await;
            match result {
                Err(Error::DeadlineExceeded | Error::Cancelled) if !started && joins_left > 0 => {
                    scope.check()?;
                    joins_left -= 1;
                }
                result => return result,
            }
        }
    }
    /// Decrypt the memory cache's retained ciphertext, the same copy an elected
    /// acquisition previously decrypted on its own. A rejected copy is
    /// invalidated, with its disk record, and reported as a miss so the caller
    /// can try pending or disk copies.
    async fn retained_plaintext(
        &self,
        page: &PageId,
        scope: &RequestScope,
        source_key: &std::cell::Cell<Option<racer_control_wire::KeyId>>,
    ) -> Result<Option<PageResult>> {
        let Some(cached) = self.metrics.lookup(
            LookupTier::Ciphertext,
            self.dependencies.memory.unverified(page),
        )?
        else {
            return Ok(None);
        };
        // Disk-sourced copies already counted their disk hit.
        if cached.disk_token.is_none() {
            self.metrics.record(Event::MemoryHit, 1);
        }
        source_key.set(Some(cached.copy.ciphertext.envelope().key_id));
        let result = async {
            let reservation = self.observe_reservation(
                scope,
                page,
                FillAdmissionSite::RetainedPlaintext,
                || self.reserve_copy_plaintext(page, &cached.copy),
            )?;
            self.call(
                DriverStage::DecryptCall,
                self.decrypt(
                    page,
                    cached.copy.clone(),
                    reservation,
                    scope,
                    DecryptSource::Retained,
                ),
            )
            .await
        }
        .await;
        match result {
            Ok(result) => {
                self.call(
                    DriverStage::PublishCall,
                    self.publish(result.clone(), None, scope),
                )
                .await?;
                Ok(Some(result))
            }
            Err(Error::CorruptRecord | Error::MissingKey) => {
                self.dependencies.memory.invalidate_ciphertext(&cached);
                if let Some(token) = &cached.disk_token {
                    self.dependencies.disk.invalidate(token)?;
                }
                Ok(None)
            }
            Err(error) => Err(error),
        }
    }

    /// Mark this detached driver's call boundary, if it owns a diagnostic generation.
    fn stage(&self, stage: DriverStage) {
        if let Some(diagnostic) = &self.diagnostic {
            diagnostic.set(stage);
        }
    }

    /// Observe one serial call; nested candidate validation deliberately has no marker.
    async fn call<T>(&self, stage: DriverStage, work: impl std::future::Future<Output = T>) -> T {
        let _call = self
            .diagnostic
            .as_ref()
            .map(|diagnostic| diagnostic.call(stage));
        let start = uring_runtime::environment::now();
        let result = work.await;
        if let Some((count, sum)) = stage_events(stage) {
            self.metrics.record_duration(count, sum, start);
        }
        result
    }

    /// Time one awaited step into a count and nanosecond sum pair.
    async fn timed<T>(
        &self,
        count: Event,
        sum: Event,
        work: impl std::future::Future<Output = T>,
    ) -> T {
        let start = uring_runtime::environment::now();
        let result = work.await;
        self.metrics.record_duration(count, sum, start);
        result
    }

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
        selection: &super::range_stream::Selection,
        membership: std::sync::Arc<crate::topology::Membership>,
        context: &OriginContext,
        scope: &RequestScope,
        budget: &mut AcquisitionBudget,
    ) -> Result<PageResult> {
        let version = selection.version.clone();
        let first = crate::model::PageNumber(
            selection
                .demand
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
        // Passthrough caches retain nothing, so a warm job left over from
        // before the switch cannot hold a usable copy.
        let warmed = if self.passthrough(&version.object.cache) {
            super::dispatch::Warmed::Unwarmed
        } else {
            self.dependencies
                .metadata_owner
                .warmed_page(&head, scope)
                .await?
        };
        match warmed {
            super::dispatch::Warmed::Local(page) => return Ok(*page),
            super::dispatch::Warmed::Unwarmed => {}
            // A warm job holds the head on a remote owner. Fill only that page
            // through the normal path, which reaches the held copy, rather
            // than selecting pages the warm job may not have finished.
            super::dispatch::Warmed::Remote => {
                return self
                    .dependencies
                    .metadata_owner
                    .acquire_unobserved(head, membership, context, scope, budget)
                    .await;
            }
        }
        // A new or credit-starved subscriber can lag behind provider completion.
        // Reuse its verified local head before asking for another node transfer.
        // The stable page owner rechecks current cache/key admission.
        if let Some(result) = self
            .dependencies
            .metadata_owner
            .local_page(head, scope)
            .await?
        {
            return Ok(result);
        }
        if let Some(response) = self
            .dependencies
            .candidates
            .subscribe(
                selection,
                super::range_stream::REQUESTED_FLIGHTS,
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
            .acquire_unobserved(
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

    #[cfg(test)]
    pub(crate) async fn accept_selected(
        &self,
        copy: crate::memory::CiphertextCopy,
        scope: &RequestScope,
    ) -> Result<PageResult> {
        self.accept_selected_inner(copy, scope, true).await
    }
    pub(crate) async fn accept_selected_unobserved(
        &self,
        copy: crate::memory::CiphertextCopy,
        scope: &RequestScope,
    ) -> Result<PageResult> {
        self.accept_selected_inner(copy, scope, false).await
    }
    async fn accept_selected_inner(
        &self,
        mut copy: crate::memory::CiphertextCopy,
        scope: &RequestScope,
        observe: bool,
    ) -> Result<PageResult> {
        scope.check()?;
        copy.validate_metadata()?;
        let page = copy.ciphertext.envelope().page.clone();
        let interest = crate::retention::Interest::default();
        let observation = self.retention_observation(&page, &interest, observe);
        if !self
            .dependencies
            .admission
            .owns(copy.ciphertext.inner.reservation())
        {
            let reservation = self.observe_reservation(
                scope,
                &page,
                FillAdmissionSite::SelectedCiphertext,
                || {
                    self.reserve_with_reclamation(
                        &page.version.object.cache,
                        ResourceClass::Ciphertext,
                        self.selected_capacity(&copy.ciphertext),
                    )
                },
            )?;
            copy.ciphertext = self
                .dependencies
                .buffers
                .rehome_aligned(copy.ciphertext, reservation)?;
        }
        let reservation =
            self.observe_reservation(scope, &page, FillAdmissionSite::SelectedPlaintext, || {
                self.reserve_copy_plaintext(&page, &copy)
            })?;
        let result = self
            .decrypt(&page, copy, reservation, scope, DecryptSource::Peer)
            .await?;
        self.publish(result.clone(), None, scope).await?;
        self.persist_verified(&result, observation);
        self.refresh_verified_background(&result, observation, scope);
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
        // At most two bounded scans, rechecking cache/global pressure each time.
        // Zero released bytes does not mean exhaustion: the memory cursor may
        // have crossed only busy or other-cache entries. No await allows new
        // local charges to interleave; remote completions can only free bytes.
        flow_control::progress::reclaim_retry(2, reserve, |error| {
            if *error != Error::Overloaded {
                return false;
            }
            let Some((owner, bytes)) = self
                .dependencies
                .admission
                .reclamation(cache, class, amount)
            else {
                return false;
            };
            // Global pressure takes over-share caches' idle bytes first.
            self.dependencies.memory.reclaim(
                class,
                owner.as_ref(),
                bytes,
                |cache| self.dependencies.admission.excess(cache, class),
                |page, ciphertext| self.dependencies.writer.discard_idle_copy(page, ciphertext),
                |cache, bytes| self.dependencies.writer.reclaim_ciphertext(cache, bytes),
            );
            true
        })
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
        // The memory cache retains no plaintext, so nothing local can be
        // reclaimed. Retry once after a yield, which lets completed readers
        // drop their plaintext.
        let mut exhausted = false;
        loop {
            scope.check()?;
            let (mut result, mut detail) =
                crate::admission::capture_rejection(admission, &mut reserve);
            if !matches!(result, Err(Error::Overloaded)) {
                return result;
            }
            let deficit = admission.reclamation(cache, ResourceClass::Plaintext, amount);
            if deficit.is_none() {
                // A remote completion may clear pressure after rejection. None
                // also means an impossible request, so retry exactly once here,
                // without restarting the scan or extending the caller's budget.
                scope.check()?;
                (result, detail) = crate::admission::capture_rejection(admission, &mut reserve);
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
            exhausted = true;
            // Always yield before the retry. Cancellation never extends the
            // original allowance.
            uring_runtime::drivers::yield_now().await;
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
        let (result, detail) =
            crate::admission::capture_rejection(&self.dependencies.admission, operation);
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
                let (result, detail) =
                    crate::admission::capture_rejection(&self.dependencies.admission, || {
                        reserve(amount)
                    });
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
        dependencies
            .writer
            .retention()
            .set_ownership(dependencies.candidates.ownership_callback());
        dependencies
            .writer
            .retention()
            .set_ownership_pass(dependencies.candidates.ownership_pass());
        Self {
            dependencies,
            metrics: Metrics::default(),
            local_copies: Rc::new(LocalCopies::default()),
            local_plaintext: Rc::new(LocalPlaintext::default()),
            local_plaintext_keys: Rc::default(),
            diagnostic: None,
            fence_acquisition: false,
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
            if self.passthrough(&version.object.cache) {
                return Ok(None);
            }
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
                .acquire_with_prefetch(page, membership, context, scope, budget, None, true, true)
                .await?
            {
                AcquiredPage::Plaintext(page) => Ok(page),
                AcquiredPage::Ciphertext(_) => Err(Error::CorruptRecord),
            }
        })
    }

    pub(crate) async fn acquire_unobserved(
        &self,
        page: PageId,
        membership: Arc<crate::topology::Membership>,
        context: &OriginContext,
        scope: &RequestScope,
        budget: &mut AcquisitionBudget,
    ) -> Result<PageResult> {
        match self
            .acquire_with_prefetch(page, membership, context, scope, budget, None, true, false)
            .await?
        {
            AcquiredPage::Plaintext(page) => Ok(page),
            AcquiredPage::Ciphertext(_) => Err(Error::CorruptRecord),
        }
    }

    /// Acquire one page for direct GPU delivery: verified plaintext when a
    /// reader still holds it on the CPU, otherwise ciphertext (from memory
    /// first, then disk, peers or origin) for the GPU to verify and
    /// decrypt. Network and disk copies keep the usual single-flight sharing,
    /// memory publication and disk persistence; only the CPU decrypt and the
    /// plaintext publication are skipped.
    pub(crate) async fn acquire_sealed(
        &self,
        page: PageId,
        membership: Arc<crate::topology::Membership>,
        context: &OriginContext,
        scope: &RequestScope,
        budget: &mut AcquisitionBudget,
        target: &super::sealed::SealedTarget,
    ) -> Result<super::sealed::Fetched> {
        use super::sealed::Fetched;
        // Verified memory, and pages another reader already authenticates,
        // keep the plaintext path: the bytes are on the CPU anyway.
        if self.cached_page(&page, scope)?.is_some() {
            return self
                .acquire(page, membership, context, scope, budget)
                .await
                .map(Fetched::Page);
        }
        // The memory cache keeps only ciphertext once readers drop the
        // plaintext. A cached copy skips the disk and peer paths and is sealed
        // for the GPU from host memory below.
        let cached = self.dependencies.memory.unverified(&page)?.is_some();
        if !cached
            && target.helper.cufile()
            && self
                .dependencies
                .writer
                .copy_only(&page)
                .ok()
                .flatten()
                .is_none()
        {
            match self.stage_from_disk(&page, scope, target).await {
                Ok(Some(sealed)) => return Ok(Fetched::Sealed(sealed)),
                Ok(None) => {}
                Err(error @ (Error::Cancelled | Error::DeadlineExceeded)) => return Err(error),
                // Any other failure reads the page through host memory.
                Err(_) => {}
            }
        }
        if !cached {
            match self
                .land_from_peer(&page, membership.clone(), context, scope, budget, target)
                .await
            {
                Ok(Some(fetched)) => return Ok(fetched),
                Ok(None) => {}
                Err(error @ (Error::Cancelled | Error::DeadlineExceeded)) => return Err(error),
                // Any other failure takes the usual path through host memory.
                Err(_) => {}
            }
        }
        let acquired = self
            .acquire_with_prefetch(
                page.clone(),
                membership.clone(),
                context,
                scope,
                budget,
                None,
                false,
                true,
            )
            .await?;
        match acquired {
            AcquiredPage::Plaintext(result) => Ok(Fetched::Page(result)),
            AcquiredPage::Ciphertext(copy) => match self.seal(&page, copy) {
                Ok(sealed) => Ok(Fetched::Sealed(sealed)),
                // A key the GPU cannot use, or an AAD too long for it: the
                // CPU authenticates the page as for a Get.
                Err(_) => self
                    .acquire(page, membership, context, scope, budget)
                    .await
                    .map(Fetched::Page),
            },
        }
    }

    /// Fetch a page from a peer straight into a GPU landing buffer (GPUDirect
    /// RDMA). This is a private fetch for one sealed read: it does not join
    /// or create a shared single flight, and a landed page is never published
    /// to memory or persisted, because there are no host bytes to share.
    ///
    /// Up to [`LANDING_PROVIDERS`] providers are asked in visit order, one
    /// CopyOnly request each; a miss or failure moves on to the next. None
    /// means use the usual path: no landing buffer on the request's GPU, a
    /// local copy exists, this node owns the page, or every provider asked
    /// missed or failed. A page a provider sent to host memory instead (no
    /// free landing slot, or HTTP) is used privately in the same way, so it
    /// is not fetched twice.
    async fn land_from_peer(
        &self,
        page: &PageId,
        membership: Arc<crate::topology::Membership>,
        context: &OriginContext,
        scope: &RequestScope,
        budget: &mut AcquisitionBudget,
        target: &super::sealed::SealedTarget,
    ) -> Result<Option<super::sealed::Fetched>> {
        let resolver = &self.dependencies.candidates;
        if !resolver.landing_ready(target.ordinal)
            || self
                .dependencies
                .writer
                .copy_only(page)
                .ok()
                .flatten()
                .is_some()
        {
            return Ok(None);
        }
        if let Some(local) = self.acquire_local_copy(page, scope, false).await? {
            return Ok(Some(match local {
                AcquiredPage::Plaintext(result) => super::sealed::Fetched::Page(result),
                AcquiredPage::Ciphertext(copy) => match self.seal(page, copy) {
                    Ok(sealed) => super::sealed::Fetched::Sealed(sealed),
                    Err(_) => return Ok(None),
                },
            }));
        }
        let candidates = resolver
            .candidates_scoped(membership.clone(), &page.version.object, page.number, scope)
            .await?;
        if resolver.is_candidate(&candidates) {
            return Ok(None);
        }
        let order = resolver.visit_order(&candidates, &page.version.object, page.number);
        let ranks: Vec<usize> = order.ranks().take(LANDING_PROVIDERS).collect();
        let mut landed = None;
        for rank in ranks {
            let destination = candidates.ordered[rank].clone();
            let delivered = match resolver
                .request_landing(
                    &membership,
                    &destination,
                    context,
                    page,
                    scope,
                    budget,
                    target.ordinal,
                )
                .await
            {
                Ok(delivered) => delivered,
                Err(error @ (Error::Cancelled | Error::DeadlineExceeded)) => return Err(error),
                // A failed provider: ask the next one.
                Err(_) => continue,
            };
            match delivered {
                crate::peer::Delivered::Landed(answer) => {
                    landed = Some(answer);
                    break;
                }
                // The provider sent the page to host memory (no free landing
                // slot, or HTTP): use it privately rather than fetch it again.
                crate::peer::Delivered::Response(response) => {
                    // A miss or unusable answer: ask the next provider.
                    let Ok(copy) = response_copy(response.response(), page) else {
                        continue;
                    };
                    return Ok(self
                        .seal(
                            page,
                            UnverifiedPage {
                                copy,
                                disk_token: None,
                            },
                        )
                        .ok()
                        .map(super::sealed::Fetched::Sealed));
                }
            }
        }
        let Some(landed) = landed else {
            return Ok(None);
        };
        let crate::peer::transport::LandedResponse {
            metadata,
            envelope,
            landed,
            ..
        } = *landed;
        if landed.ordinal() != target.ordinal || envelope.page != *page {
            return Ok(None);
        }
        metadata.immutable().validate_page(&envelope)?;
        let sealed = Box::new(super::sealed::SealedPage {
            aad: crate::security::page_aad(&envelope)?,
            key: self.dependencies.crypto.derive_page_key(&envelope)?,
            // No host bytes to checksum: the GPU checks the AEAD tag.
            expected_crc: None,
            metadata,
            envelope,
            source: super::sealed::SealedSource::Landed(landed),
        });
        sealed.validate_for(page)?;
        self.metrics.record(Event::PeerHit, 1);
        Ok(Some(super::sealed::Fetched::Sealed(sealed)))
    }

    /// Package host ciphertext for GPU verification.
    fn seal(&self, page: &PageId, copy: UnverifiedPage) -> Result<Box<super::sealed::SealedPage>> {
        validate_copy(&copy.copy, page)?;
        let envelope = copy.copy.ciphertext.envelope().clone();
        let sealed = Box::new(super::sealed::SealedPage {
            metadata: copy.copy.metadata.clone(),
            aad: crate::security::page_aad(&envelope)?,
            key: self.dependencies.crypto.derive_page_key(&envelope)?,
            expected_crc: copy.copy.ciphertext.cached_checksum(),
            envelope,
            source: super::sealed::SealedSource::Host(copy),
        });
        sealed.validate_for(page)?;
        Ok(sealed)
    }

    /// Read a local v2 record's ciphertext straight into GPU staging with
    /// cuFile. Only the header passes through host memory. None means the
    /// page must be read through host memory: a miss, a v1 or unaligned
    /// record, no free staging slot, or cuFile unavailable.
    async fn stage_from_disk(
        &self,
        page: &PageId,
        scope: &RequestScope,
        target: &super::sealed::SealedTarget,
    ) -> Result<Option<Box<super::sealed::SealedPage>>> {
        if target.helper.free_slots(target.ordinal) == 0 {
            target
                .helper
                .counters()
                .exhausted
                .fetch_add(1, std::sync::atomic::Ordering::Relaxed);
            return Ok(None);
        }
        let Some(record) = self
            .dependencies
            .disk
            .direct_read(page, scope, |amount| {
                self.reserve_with_reclamation(
                    &page.version.object.cache,
                    ResourceClass::Ciphertext,
                    amount,
                )
            })
            .await?
        else {
            return Ok(None);
        };
        if !record.ciphertext.aligned {
            return Ok(None);
        }
        let Some(fd) = host_descriptor(&record.ciphertext.descriptor) else {
            return Ok(None);
        };
        let envelope = record.envelope.clone();
        if record.ciphertext.length != envelope.ciphertext_length as usize {
            return Ok(None);
        }
        let aad = crate::security::page_aad(&envelope)?;
        let key = self.dependencies.crypto.derive_page_key(&envelope)?;
        let read_length = record.read_length;
        let staged = target
            .helper
            .stage_file(
                target.ordinal,
                fd,
                record.ciphertext.offset,
                read_length as u64,
                scope,
            )
            .await;
        // The read finished (or was abandoned): the bytes may now be recycled.
        let crate::store::DirectRecord {
            token,
            metadata,
            checksum,
            lease,
            ..
        } = record;
        drop(lease);
        let staged = match staged {
            Ok(staged) => staged,
            Err(error @ (Error::Cancelled | Error::DeadlineExceeded)) => return Err(error),
            Err(_) => return Ok(None),
        };
        let sealed = Box::new(super::sealed::SealedPage {
            metadata,
            envelope,
            aad,
            key,
            expected_crc: Some(checksum),
            source: super::sealed::SealedSource::Staged { staged, token },
        });
        sealed.validate_for(page)?;
        self.metrics.record(Event::DiskHit, 1);
        self.metrics.record(Event::GpuDirectDiskRead, 1);
        self.metrics
            .record(Event::GpuDirectDiskReadBytes, read_length as u64);
        Ok(Some(sealed))
    }

    /// Act on the GPU's verdict for a sealed page this worker supplied: a
    /// rejected copy is dropped from memory and disk, as a failed CPU decrypt
    /// would drop it; an authenticated network copy may now be persisted.
    pub(crate) fn sealed_verdict(&self, report: super::sealed::SealedReport, passed: bool) {
        let super::sealed::SealedReport { page, copy, token } = report;
        if passed {
            if let Some(copy) = copy
                && copy.disk_token.is_none()
            {
                let interest = crate::retention::Interest::default();
                let observation = self.retention_observation(&page, &interest, false);
                self.persist_copy(copy.copy, observation);
            }
            return;
        }
        self.metrics.record(Event::CorruptMiss, 1);
        if let Some(copy) = &copy {
            self.dependencies.memory.invalidate_ciphertext(copy);
            if let Some(token) = &copy.disk_token {
                let _ = self.dependencies.disk.invalidate(token);
            }
        }
        if let Some(token) = &token {
            let _ = self.dependencies.disk.invalidate(token);
        }
    }

    /// Warm gates own transfer exclusion until accepted acquisition work is fenced.
    pub(crate) async fn acquire_warm(
        &self,
        page: PageId,
        membership: Arc<crate::topology::Membership>,
        context: &OriginContext,
        scope: &RequestScope,
        budget: &mut AcquisitionBudget,
    ) -> Result<PageResult> {
        let mut fill = self.clone();
        fill.fence_acquisition = true;
        fill.acquire_unobserved(page, membership, context, scope, budget)
            .await
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
                .acquire_with_prefetch(page, membership, context, scope, budget, None, false, true)
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
                    false,
                )
                .await?
            {
                AcquiredPage::Plaintext(page) => Ok(page),
                AcquiredPage::Ciphertext(_) => Err(Error::CorruptRecord),
            }
        })
    }

    /// Acquire one page with the caller's prefetch, budget, and observation policy.
    #[allow(clippy::too_many_arguments)] // Keep acquisition inputs and borrowed budget explicit.
    fn acquire_with_prefetch<'a>(
        &'a self,
        page: PageId,
        membership: std::sync::Arc<crate::topology::Membership>,
        context: &'a OriginContext,
        scope: &'a RequestScope,
        budget: &'a mut AcquisitionBudget,
        mut prefetch: Option<crate::origin::OriginPage>,
        plaintext: bool,
        observe: bool,
    ) -> Operation<'a, AcquiredPage> {
        Box::pin(async move {
            scope.check()?;
            if page.version.object != context.object {
                return Err(Error::InvalidRequest);
            }
            // Plaintext another reader still holds is already deliverable.
            // Optional cold ownership ranking must not introduce a
            // yield/deadline failure before it. Cached ciphertext without live
            // plaintext is decrypted by the elected flight below.
            let interest = crate::retention::Interest::default();
            if let Some(result) = self
                .metrics
                .lookup(LookupTier::Plaintext, self.dependencies.memory.get(&page))?
            {
                result.validate_for(&page)?;
                self.metrics.record(Event::MemoryHit, 1);
                let observation = self.retention_observation(&page, &interest, observe);
                self.persist_verified(&result, observation);
                self.refresh_verified_background(&result, observation, scope);
                return Ok(result.into());
            }
            // Each independent follower observes before joining. Retries inside
            // this operation reuse this decision, never their own inserted bit.
            let _ = self
                .dependencies
                .candidates
                .refresh_ownership_scoped(&page, scope)
                .await;
            scope.check()?;
            let observation = self.retention_observation(&page, &interest, observe);
            let joined = if plaintext && prefetch.is_none() {
                self.dependencies
                    .flights
                    .join_wait(page.clone(), membership, context, scope, budget)
                    .await
            } else {
                self.dependencies.flights.join_for(
                    page.clone(),
                    membership,
                    context,
                    scope,
                    budget,
                    plaintext,
                )
            };
            let mut waiter = match joined? {
                JoinedFlight::Complete(result) => {
                    // join checks current UID/key admission before sharing a
                    // completed bundle; registered waiters retain their own rights.
                    result.validate_for(&page)?;
                    self.persist_verified(&result, observation);
                    return Ok(result.into());
                }
                JoinedFlight::Ciphertext(copy) => return Ok(AcquiredPage::Ciphertext(copy)),
                JoinedFlight::Waiter(waiter) => waiter,
            };
            loop {
                let event = waiter.wait().await;
                if self.fence_acquisition
                    && matches!(event, Err(_) | Ok(AcquisitionEvent::Failed(_)))
                {
                    waiter.wait_operations().await?;
                }
                match event? {
                    AcquisitionEvent::Complete(result) => {
                        result.validate_for(&page)?;
                        self.persist_verified(&result, observation);
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
                            observation,
                        )
                        .await?;
                    }
                }
            }
        })
    }

    /// Transfer elected work to the worker driver. The caller only waits for its
    /// remaining budget; dropping that wait cannot release accepted work before
    /// the operation completes.
    // Keep the reader's latched observation explicit at the driver handoff rather
    // than re-observing retries or coupling it to the flight's ownership guards.
    #[allow(clippy::too_many_arguments)]
    async fn drive_elected_acquisition(
        &self,
        page: &PageId,
        leader: FlightLeader,
        waiter: &mut AcquisitionWaiter<'_>,
        prefetch: &mut Option<crate::origin::OriginPage>,
        plaintext: bool,
        observation: crate::retention::Observation,
    ) -> Result<()> {
        use std::future::Future;

        let mut shared_local = false;
        let retained = self.dependencies.flights.ciphertext_for(&leader)?;
        // racer-core orders the local sources. Cached pages retain only
        // ciphertext; a plaintext acquisition decrypts it through the shared
        // local plaintext job, so range probes and warm readers of the same
        // page share one reservation and one decrypt in either arrival order.
        let local_job = || self.local_plaintext.get(page).is_some();
        let unverified =
            || -> Result<bool> { Ok(self.dependencies.memory.unverified(page)?.is_some()) };
        let source = racer_core::read::leader_source(
            retained.is_some(),
            plaintext,
            plaintext && local_job(),
            plaintext && !local_job() && retained.is_none() && unverified()?,
        );
        let ready = match source {
            LeaderSource::Retained => retained,
            LeaderSource::SharedLocal => {
                shared_local = true;
                None
            }
            LeaderSource::Cached => {
                let cached = self.metrics.lookup(
                    LookupTier::Ciphertext,
                    self.dependencies.memory.unverified(page),
                )?;
                // Disk-sourced copies already counted their disk hit.
                if cached
                    .as_ref()
                    .is_some_and(|cached| cached.disk_token.is_none())
                {
                    self.metrics.record(Event::MemoryHit, 1);
                }
                cached
            }
        };
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
        // Unless the caller fences on this work (warm gates), the running fill continues for
        // another live acquisition waiter when the leader expires or is canceled, instead of being
        // canceled and restarted from scratch by the next leader. The owned scope then runs to a
        // hard ceiling; the driver cancels it as soon as no live waiter remains
        // (`FlightOperation::poll_cancellation`). Attempts and links stay charged to the leader's
        // budget; the extra time is within the adopting waiter's own deadline and budget.
        let handover = !self.fence_acquisition;
        let ceiling = racer_core::read::flight::handover_ceiling(
            uring_runtime::environment::now(),
            caller_scope.deadline.0,
            handover,
        );
        let owned_scope = RequestScope::new(caller_scope.request, ceiling)?;
        let mut operation = self.dependencies.flights.retain_driver_operation(
            &leader,
            self.metrics.lease(Gauge::ActiveFills)?,
            handover,
        )?;
        let mut owned_budget = acquisition.budget.transfer();
        let owned_membership = acquisition.membership.clone();
        let owned_page = page.clone();
        let prefetched = prefetch.take();
        let mut fill = self.clone();
        fill.diagnostic = self.dependencies.flights.driver_diagnostic(&leader);
        let diagnostic_lifetime = DriverDiagnostic::lifetime(fill.diagnostic.clone());
        let flights = self.dependencies.flights.clone();
        let (send, mut receive) = futures::channel::oneshot::channel();
        let submitted = uring_runtime::environment::now();
        driver_permit.submit_detached(Box::pin(async move {
            fill.metrics.record_duration(
                Event::FillDriverStartCount,
                Event::FillDriverStartNs,
                submitted,
            );
            let started = uring_runtime::environment::now();
            let _diagnostic_lifetime = diagnostic_lifetime;
            let mut work = Box::pin(async {
                if shared_local {
                    // A local miss or a rejected local copy falls through to
                    // ordinary acquisition with this flight's own rights.
                    match fill
                        .local_plaintext_copy(&owned_page, &owned_scope, false)
                        .await
                    {
                        Ok(Some(result)) => return Ok(result.into()),
                        Ok(None) | Err(Error::MissingKey | Error::Unavailable) => {}
                        Err(error) => return Err(error),
                    }
                }
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
                        fill.call(
                            DriverStage::DecryptCall,
                            fill.decrypt(
                                &owned_page,
                                copy.copy.clone(),
                                reservation,
                                &owned_scope,
                                DecryptSource::Retained,
                            ),
                        )
                        .await
                    }
                    .await;
                    match result {
                        Ok(result) => {
                            fill.call(
                                DriverStage::PublishCall,
                                fill.publish(result.clone(), None, &owned_scope),
                            )
                            .await?;
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
                    && ((!handover && caller_scope.check().is_err())
                        || operation.poll_cancellation(cx))
                {
                    if let Some(diagnostic) = &fill.diagnostic {
                        diagnostic.cancellation_seen();
                    }
                    let _ = owned_scope.cancel();
                }
                work.as_mut().poll(cx)
            })
            .await;
            fill.stage(DriverStage::WorkReturned);
            fill.metrics
                .record_duration(Event::FillDriverCount, Event::FillDriverNs, started);
            drop(work);
            operation.complete()?;
            match result {
                Ok(result) => {
                    if let AcquiredPage::Plaintext(page) = &result {
                        // Origin can produce verified plaintext for a ciphertext
                        // requester. Keep the original reader's latched decision.
                        fill.persist_verified(page, observation);
                    }
                    if let AcquiredPage::Ciphertext(copy) = &result {
                        match fill.dependencies.memory.publish_ciphertext(copy.clone()) {
                            Ok(()) => {}
                            Err(error)
                                if racer_core::read::Error::try_from(error)
                                    .is_ok_and(racer_core::read::ciphertext_publish_tolerated) => {}
                            Err(error) => return Err(error),
                        }
                    }
                    let _ = flights.publish_acquired(leader, result);
                }
                Err(error) => {
                    use racer_core::read::FailureKind;
                    let failure = match racer_core::read::Error::try_from(error)
                        .map(racer_core::read::acquisition_failure)
                    {
                        Ok(FailureKind::OriginRejected) => AcquisitionFailure::OriginRejected,
                        Ok(FailureKind::OriginForbidden) => AcquisitionFailure::OriginForbidden,
                        _ => AcquisitionFailure::Terminal(error),
                    };
                    let _ = flights.fail(leader, failure);
                }
            }
            let _ = send.send(owned_budget);
            Ok(())
        }));
        let remaining = std::future::poll_fn(|cx| {
            uring_runtime::drivers::poll(cx, 64);
            if !self.fence_acquisition || !acquisition.scope.cancellation.is_cancelled() {
                acquisition.cancellation.register(cx.waker());
            }
            if !self.fence_acquisition {
                acquisition.scope.check()?;
            }
            std::pin::Pin::new(&mut receive)
                .poll(cx)
                .map(|result| result.map_err(|_| Error::Unavailable))
        })
        .await?;
        *acquisition.budget = remaining;
        if self.fence_acquisition {
            acquisition.scope.check()?;
        }
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
        let mode = self.mode(&page.version.object.cache);
        if mode == racer_control_wire::CacheMode::PassthroughLocal {
            // A local-route bootstrap comes from this node's own origin.
            self.dependencies.candidates.local_authority(
                membership,
                &page.version.object,
                page.number,
                mode,
            )?;
        } else {
            let candidates = self
                .call(
                    DriverStage::CandidateCompositeCall,
                    self.dependencies.candidates.candidates_scoped(
                        membership,
                        &page.version.object,
                        page.number,
                        scope,
                    ),
                )
                .await?;
            if !self.dependencies.candidates.is_candidate(&candidates) {
                return Err(Error::Unauthorized);
            }
        }
        let ciphertext =
            self.observe_reservation(scope, page, FillAdmissionSite::OriginCiphertext, || {
                self.reserve_with_reclamation(
                    &page.version.object.cache,
                    ResourceClass::Ciphertext,
                    PAGE_BYTES as usize + 16,
                )
            })?;
        let (plaintext, ciphertext) = self
            .call(
                DriverStage::EncryptCall,
                self.dependencies
                    .crypto
                    .encrypt(page.clone(), origin.plaintext, ciphertext, scope),
            )
            .await?;
        let result = PageResult {
            metadata: origin.metadata,
            plaintext,
            ciphertext,
        };
        result.validate_for(page)?;
        self.call(
            DriverStage::PublishCall,
            self.publish(result.clone(), None, scope),
        )
        .await?;
        self.metrics.record(Event::OriginFill, 1);
        Ok(result)
    }
    /// Whether this node holds a copy of `page` in memory, pending write, or
    /// the disk index, without reading it (orphan notices and copy offers).
    /// Lookup errors count as not held.
    pub(crate) fn holds(&self, page: &PageId) -> bool {
        let deps = &self.dependencies;
        // A passthrough page is never retained, so it is never offered.
        !self.passthrough(&page.version.object.cache)
            && (deps.memory.ciphertext(page).is_ok_and(|c| c.is_some())
                || deps.writer.copy_only(page).is_ok_and(|c| c.is_some())
                || deps.writer.index().lookup(page).is_ok_and(|c| c.is_some()))
    }

    /// Strictly local completed/pending copy or join of existing work; never starts
    /// another acquisition or contacts origin. Use Flights::join_copy, whose waiter
    /// has no election/context API. Ciphertext preserves its nonce/tag.
    pub fn copy_only<'a>(
        &'a self,
        page: &'a PageId,
        scope: &'a RequestScope,
    ) -> Operation<'a, Option<(ObjectMetadata, CiphertextPage)>> {
        self.copy_only_joining(page, scope, true)
    }

    /// [`Self::copy_only`]; without `join` a running acquisition of the page is a miss instead of
    /// something to wait on.
    pub fn copy_only_joining<'a>(
        &'a self,
        page: &'a PageId,
        scope: &'a RequestScope,
        join: bool,
    ) -> Operation<'a, Option<(ObjectMetadata, CiphertextPage)>> {
        Box::pin(async move {
            scope.check()?;
            // A passthrough page can only be shared while it is in flight.
            let retained = !self.passthrough(&page.version.object.cache);
            if retained
                && let Some(copy) = self.metrics.lookup(
                    LookupTier::Ciphertext,
                    self.dependencies.memory.ciphertext(page),
                )?
            {
                validate_copy(&copy, page)?;
                self.metrics.record(Event::MemoryHit, 1);
                return Ok(Some((copy.metadata, copy.ciphertext)));
            }
            if retained
                && let Some(copy) = self.metrics.lookup(
                    LookupTier::Pending,
                    self.dependencies.writer.copy_only(page),
                )?
            {
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
                Ok(None) | Err(Error::MissingKey | Error::Io | Error::Os(_)) => {}
                Err(error) => return Err(error),
            }
            if !join {
                return Ok(None);
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
        if self.passthrough(&page.version.object.cache) {
            return Ok(None);
        }
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
            let flight = self.dependencies.flights.reserve_copy_flight(page, scope)?;
            let owned_scope = RequestScope::new(scope.request, scope.deadline.0)?;
            let (receive, completion) = self
                .local_copies
                .start(page.clone(), Err(Error::Unavailable));
            if let Some(completion) = completion {
                let mut fill = self.clone();
                fill.diagnostic = None;
                let page = page.clone();
                permit.submit_detached(Box::pin(async move {
                    let _flight = flight;
                    let result = async {
                        let Some((copy, token)) =
                            fill.read_disk_observed(&page, &owned_scope).await?
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
            }
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
        self.acquire_local_copy_from(page, scope, want_plaintext, None)
            .await
    }

    /// Record the chosen copy's key in `source_key` before decrypting it, so shared
    /// local plaintext work can admit new followers against that key.
    async fn acquire_local_copy_from(
        &self,
        page: &PageId,
        scope: &RequestScope,
        want_plaintext: bool,
        source_key: Option<&std::cell::Cell<Option<racer_control_wire::KeyId>>>,
    ) -> Result<Option<AcquiredPage>> {
        scope.check()?;
        // Residual copies from before a switch to passthrough are never served.
        if self.passthrough(&page.version.object.cache) {
            return Ok(None);
        }
        // Pending copies already own ciphertext. Disk owns a separate complete
        // staging/decoded bundle; neither source needs speculative network bytes.
        // The memory cache retains only ciphertext, so a page whose plaintext
        // was released is decrypted from memory before trying disk.
        let mut cached = None;
        let local = match self.metrics.lookup(
            LookupTier::Pending,
            self.dependencies.writer.copy_only(page),
        )? {
            Some(copy) => Some(copy),
            None => {
                cached = self.metrics.lookup(
                    LookupTier::Ciphertext,
                    self.dependencies.memory.unverified(page),
                )?;
                cached.as_ref().map(|cached| cached.copy.clone())
            }
        };
        let (local, token) = match local {
            Some(copy) => (Some(copy), None),
            None => {
                match self
                    .call(
                        DriverStage::DiskReadCall,
                        self.read_disk_observed(page, scope),
                    )
                    .await
                {
                    Ok(Some((copy, token))) => (Some(copy), Some(token)),
                    Ok(None)
                    | Err(Error::CorruptRecord | Error::MissingKey | Error::Io | Error::Os(_)) => {
                        (None, None)
                    }
                    Err(error) => return Err(error),
                }
            }
        };
        if let Some(copy) = local {
            if !want_plaintext && let Some(token) = &token {
                match self
                    .call(
                        DriverStage::ChecksumCall,
                        self.validate_disk_copy(&copy, page, token, scope),
                    )
                    .await
                {
                    Ok(()) => {}
                    Err(Error::CorruptRecord) => {
                        self.metrics.record(Event::CorruptMiss, 1);
                        return Ok(None);
                    }
                    Err(Error::MissingKey) => return Ok(None),
                    Err(error) => return Err(error),
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
            if let Some(source_key) = source_key {
                source_key.set(Some(copy.ciphertext.envelope().key_id));
            }
            let result = async {
                scope.check()?;
                let reservation = self.observe_reservation(
                    scope,
                    page,
                    FillAdmissionSite::LocalPlaintext,
                    || self.reserve_copy_plaintext(page, &copy),
                )?;
                self.call(
                    DriverStage::DecryptCall,
                    self.decrypt(page, copy, reservation, scope, source),
                )
                .await
            }
            .await;
            match result {
                Ok(result) => {
                    self.call(
                        DriverStage::PublishCall,
                        self.publish(result.clone(), None, scope),
                    )
                    .await?;
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
                    if let Some(cached) = &cached {
                        self.dependencies.memory.invalidate_ciphertext(cached);
                        // A cached copy may have been read from disk; drop the
                        // rejected disk record too so it cannot be served again.
                        if let Some(token) = &cached.disk_token {
                            self.dependencies.disk.invalidate(token)?;
                        }
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

    /// Drive the read's racer-core acquisition
    /// ([`super::candidates::CandidatePolicy::drive`]): it decides each step,
    /// and this loop performs the hedge, origin fetch, and publication.
    /// Errors raised by the I/O itself return directly.
    pub(super) async fn acquire_once(
        &self,
        page: &PageId,
        membership: std::sync::Arc<crate::topology::Membership>,
        context: &OriginContext,
        scope: &RequestScope,
        budget: &mut AcquisitionBudget,
        want_plaintext: bool,
    ) -> Result<AcquiredPage> {
        use racer_core::read::Source;
        use racer_core::read::walk::{Action, Event as FillEvent};
        let mode = self.mode(&page.version.object.cache);
        if mode == racer_control_wire::CacheMode::PassthroughLocal {
            return self
                .acquire_local_origin(
                    page,
                    membership,
                    context,
                    scope,
                    budget,
                    want_plaintext,
                    mode,
                )
                .await;
        }
        if let Some(local) = self.acquire_local_copy(page, scope, want_plaintext).await? {
            return Ok(local);
        }
        let candidates = self
            .timed(
                Event::FillCandidatesCount,
                Event::FillCandidatesNs,
                self.call(
                    DriverStage::CandidateCompositeCall,
                    self.dependencies.candidates.candidates_scoped(
                        membership,
                        &page.version.object,
                        page.number,
                        scope,
                    ),
                ),
            )
            .await?;
        // Cipher-only acquisitions reserve no plaintext until origin actually
        // supplies a page. Requester consumers still authenticate before acceptance.
        let mut plaintext = None;
        if want_plaintext {
            plaintext = Some(
                self.call(
                    DriverStage::PlaintextReclaim,
                    self.reserve_network_plaintext(scope, page),
                )
                .await?,
            );
        }
        let operation = PeerOperation::Page {
            page: page.clone(),
            mode: FetchMode::Acquire,
        };
        let peer_page = |response, reserved| {
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
            }) as Operation<'_, AcquiredPage>
        };
        let policy = &self.dependencies.candidates;
        let mut acquisition =
            policy.acquisition(candidates, &operation, scope, want_plaintext, false);
        let mut acquired = None;
        let mut event = FillEvent::Start { local_hit: false };
        loop {
            let action = self
                .call(
                    DriverStage::CandidateCompositeCall,
                    Box::pin(policy.drive(
                        &mut acquisition,
                        event,
                        context,
                        &operation,
                        scope,
                        budget,
                        &mut |response| peer_page(response, plaintext.take()),
                        &mut acquired,
                    )),
                )
                .await?;
            event = match action {
                Action::Hedge {
                    eligible,
                    first,
                    second,
                } => {
                    let hedge_start = uring_runtime::environment::now();
                    let (result, end) = self
                        .timed(
                            Event::FillHedgeCount,
                            Event::FillHedgeNs,
                            self.call(
                                DriverStage::CandidateCompositeCall,
                                policy.hedge_page(
                                    acquisition.candidates(),
                                    (first, second, eligible),
                                    context,
                                    &operation,
                                    scope,
                                    budget,
                                    &self.dependencies.admission,
                                    |response, child| {
                                        Box::pin(async move {
                                            self.metrics.record_duration(
                                                Event::FillPeerResponseCount,
                                                Event::FillPeerResponseNs,
                                                hedge_start,
                                            );
                                            let copy = response_copy(response.response(), page)?;
                                            if let Some(existing) = self
                                                .retained_metadata(&page.version, &child)
                                                .await?
                                                && !existing.compatible(&copy.metadata.immutable())
                                            {
                                                return Err(Error::CorruptRecord);
                                            }
                                            let result = self
                                                .timed(
                                                    Event::FillPeerDecryptCount,
                                                    Event::FillPeerDecryptNs,
                                                    self.decrypt_response(
                                                        page, response, None, &child,
                                                    ),
                                                )
                                                .await?;
                                            // A sibling may publish while crypto runs. Recheck before
                                            // election, not only after a winner has canceled its peer.
                                            if let Some(existing) = self
                                                .retained_metadata(&page.version, &child)
                                                .await?
                                                && !existing
                                                    .compatible(&result.metadata.immutable())
                                            {
                                                return Err(Error::CorruptRecord);
                                            }
                                            Ok(result)
                                        })
                                    },
                                ),
                            ),
                        )
                        .await?;
                    acquisition.trail.hedge_history_unavailable =
                        !matches!(end, racer_core::read::walk::HedgeEnd::NotStarted);
                    acquired = result.map(AcquiredPage::from);
                    FillEvent::Hedge(end)
                }
                Action::Origin { rank } => {
                    let authority = policy.authority(&mut acquisition, rank, &operation);
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
                        .call(
                            DriverStage::OriginCall,
                            self.dependencies
                                .origin
                                .page_reserved(authority, context, page, plaintext, scope),
                        )
                        .await
                    {
                        Ok(origin) => {
                            // An owner after rank 0 holds a copy rank 0 may not know of: offer it
                            // later. The authority's owners: a walk that refreshed its membership
                            // ranks under the newer one.
                            if policy.note_orphan(
                                page,
                                authority.owners(),
                                authority.rank(),
                                authority.predecessor_evidence(),
                            ) {
                                self.metrics.record(Event::OrphanNotice, 1);
                            }
                            acquired = Some(
                                self.encrypt_origin_page(page, origin, ciphertext, scope)
                                    .await?
                                    .into(),
                            );
                            FillEvent::Origin(Ok(()))
                        }
                        Err(error) => {
                            if error != Error::VersionUnavailable {
                                policy.note_origin_failure(
                                    page,
                                    authority.owners(),
                                    authority.rank(),
                                    &error,
                                );
                            }
                            FillEvent::Origin(Err(error.try_into()?))
                        }
                    }
                }
                Action::Publish(_) => {
                    let result = acquired.as_ref().expect("acquired page");
                    result.validate_for(page)?;
                    scope.check()?;
                    if let AcquiredPage::Plaintext(page) = result {
                        // Origin encryption already produced a verified whole page. Preserve
                        // that evidence even when the supplier requested only ciphertext.
                        self.call(
                            DriverStage::PublishCall,
                            self.publish(page.clone(), None, scope),
                        )
                        .await?;
                    }
                    FillEvent::Published(Ok(()))
                }
                Action::Done(Ok(source)) => {
                    match source {
                        Source::Local => {}
                        Source::Origin => self.metrics.record(Event::OriginFill, 1),
                        Source::Hedge | Source::Peer => self.metrics.record(Event::PeerHit, 1),
                    }
                    return Ok(acquired.expect("acquired page"));
                }
                action => unreachable!("the acquisition driver performs {action:?}"),
            };
        }
    }

    /// Fill `page` from this node's own origin socket, never asking peers. Only a
    /// passthrough cache on the local route reaches here; concurrent readers on
    /// this worker already share this call through the page flight.
    #[allow(clippy::too_many_arguments)] // Mirrors acquire_once's borrowed inputs.
    async fn acquire_local_origin(
        &self,
        page: &PageId,
        membership: std::sync::Arc<crate::topology::Membership>,
        context: &OriginContext,
        scope: &RequestScope,
        budget: &mut AcquisitionBudget,
        want_plaintext: bool,
        mode: racer_control_wire::CacheMode,
    ) -> Result<AcquiredPage> {
        let authority = self.dependencies.candidates.local_authority(
            membership,
            &page.version.object,
            page.number,
            mode,
        )?;
        authority.validate(&context.object, page.number)?;
        let plaintext = if want_plaintext {
            self.call(
                DriverStage::PlaintextReclaim,
                self.reserve_network_plaintext(scope, page),
            )
            .await?
        } else {
            self.observe_reservation(scope, page, FillAdmissionSite::OriginPlaintext, || {
                self.reserve_bootstrap(&context.object.cache)
            })?
        };
        budget.begin_attempt(uring_runtime::environment::now(), scope.deadline.0)?;
        scope.check()?;
        let ciphertext =
            self.observe_reservation(scope, page, FillAdmissionSite::OriginCiphertext, || {
                self.reserve_with_reclamation(
                    &context.object.cache,
                    ResourceClass::Ciphertext,
                    PAGE_BYTES as usize + 16,
                )
            })?;
        let origin = self
            .call(
                DriverStage::OriginCall,
                self.dependencies
                    .origin
                    .page_reserved(&authority, context, page, plaintext, scope),
            )
            .await?;
        let result = self
            .encrypt_origin_page(page, origin, ciphertext, scope)
            .await?;
        result.validate_for(page)?;
        self.metrics.record(Event::OriginFill, 1);
        self.metrics.record(Event::PassthroughLocalOrigin, 1);
        Ok(result.into())
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
            .call(
                DriverStage::EncryptCall,
                self.dependencies
                    .crypto
                    .encrypt(page.clone(), origin.plaintext, ciphertext, scope),
            )
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

    /// Charge for rehoming selected ciphertext. Pages that must be copied
    /// (registered or shared) are charged the pooled aligned size class so the
    /// copy lands in this worker's pool; exclusive pages move in place under a
    /// charge for their current capacity.
    fn selected_capacity(&self, ciphertext: &crate::memory::CiphertextPage) -> usize {
        let capacity = ciphertext.inner.capacity();
        if !ciphertext.is_registered() && Arc::strong_count(&ciphertext.inner) == 1 {
            return capacity;
        }
        self.dependencies
            .buffers
            .aligned_capacity(ciphertext.bytes().len())
            .map_or(capacity, |aligned| aligned.max(capacity))
    }

    /// Copy an authenticated registered RDMA page once into pooled aligned
    /// storage before it is published and persisted, so the memory cache and
    /// the store share one page and the RDMA buffer returns to its pool.
    ///
    /// Copy-out admission is optional: when it is refused even after
    /// reclamation, the registered page is kept and the memory cache and
    /// store copy it out under their own admission as before.
    fn detach_registered(
        &self,
        ciphertext: crate::memory::CiphertextPage,
    ) -> crate::memory::CiphertextPage {
        if !ciphertext.is_registered() {
            return ciphertext;
        }
        let buffers = &self.dependencies.buffers;
        let Ok(capacity) = buffers.aligned_capacity(ciphertext.bytes().len()) else {
            return ciphertext;
        };
        let cache = ciphertext.envelope().page.version.object.cache.clone();
        let Ok(reservation) =
            self.reserve_with_reclamation(&cache, ResourceClass::Ciphertext, capacity)
        else {
            return ciphertext;
        };
        // Keep a handle so a failed copy falls back to the registered page.
        let fallback = ciphertext.clone();
        match buffers.detach_registered(ciphertext, reservation) {
            Ok(detached) => {
                drop(fallback);
                detached
            }
            Err(_) => fallback,
        }
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
                ciphertext: self.detach_registered(copy.ciphertext),
            };
            result.validate_for(page)?;
            Ok(result)
        }
        .await;
        source.observe(&self.metrics, result)
    }

    /// Only verified results reach this boundary. Publication never re-queries
    /// Bloom history, and optional write admission cannot change read success.
    pub(crate) async fn observe_verified_scoped(&self, result: &PageResult, scope: &RequestScope) {
        if scope.check().is_err() {
            return;
        }
        let _ = self
            .dependencies
            .candidates
            .refresh_ownership_scoped(result.plaintext.page(), scope)
            .await;
        if scope.check().is_ok() {
            self.observe_verified(result, scope);
        }
    }

    pub(crate) fn observe_verified(&self, result: &PageResult, _scope: &RequestScope) {
        let observation = self.retention_observation(
            result.plaintext.page(),
            &crate::retention::Interest::default(),
            true,
        );
        self.persist_verified(result, observation);
    }

    fn refresh_verified_background(
        &self,
        result: &PageResult,
        observation: crate::retention::Observation,
        scope: &RequestScope,
    ) {
        if self.passthrough(&result.metadata.version.object.cache)
            || self
                .dependencies
                .writer
                .retention()
                .owned_only(result.plaintext.page())
                .eligible()
        {
            return;
        }
        let Ok(permit) = uring_runtime::drivers::reserve() else {
            return;
        };
        let mut fill = self.clone();
        fill.diagnostic = None;
        let result = result.clone();
        let scope = scope.clone();
        permit.submit_detached(Box::pin(async move {
            if scope.check().is_err() {
                return Ok::<(), Error>(());
            }
            let _ = fill
                .dependencies
                .candidates
                .refresh_ownership_scoped(result.plaintext.page(), &scope)
                .await;
            if scope.check().is_ok() {
                // Never query/insert Bloom history twice for the same interest.
                // Only a now-current owned classification can additionally grant
                // first-sight persistence; the original second-sight latch stays.
                let decision = if observation.eligible() {
                    observation
                } else {
                    fill.dependencies
                        .writer
                        .retention()
                        .owned_only(result.plaintext.page())
                };
                fill.persist_verified(&result, decision);
            }
            Ok(())
        }));
    }

    fn persist_verified(&self, result: &PageResult, observation: crate::retention::Observation) {
        if !observation.eligible() || result.validate_metadata().is_err() {
            return;
        }
        self.persist_copy(result.copy(), observation);
    }

    /// Queue an authenticated copy for disk under the reader's latched
    /// retention decision. The caller must have authenticated the bytes.
    fn persist_copy(
        &self,
        copy: crate::memory::CiphertextCopy,
        observation: crate::retention::Observation,
    ) {
        if !observation.eligible() || copy.validate_metadata().is_err() {
            return;
        }
        // Defense in depth: a passthrough cache never writes to disk, even
        // under a decision latched before its mode changed.
        if self.passthrough(&copy.metadata.version.object.cache) {
            return;
        }
        let id = &copy.ciphertext.envelope().page.clone();
        if self
            .dependencies
            .writer
            .copy_only(id)
            .ok()
            .flatten()
            .is_some()
        {
            return;
        }
        if self
            .dependencies
            .writer
            .index()
            .lookup(id)
            .ok()
            .flatten()
            .is_some()
        {
            return;
        }
        let cache = &copy.metadata.version.object.cache.clone();
        let retention = self.dependencies.writer.retention();
        // Ownership is current here, but second-sight eligibility remains the
        // latched observation. Reclassification cannot self-promote Bloom state.
        let owned = retention.owned_only(id).eligible();
        let admission = &self.dependencies.admission;
        let amount = copy.ciphertext.bytes().len();
        // Optional pages of an over-share cache do not displace other caches'
        // data once free segments reach the reserve. Owned pages compete
        // through eviction instead.
        if !owned && self.dependencies.writer.over_share_under_pressure(cache) {
            retention.share_rejected();
            retention.persistence_attempt(false);
            return;
        }
        if !owned
            && !optional_headroom(
                admission.limit(ResourceClass::DirtyCiphertext),
                admission.used(ResourceClass::DirtyCiphertext),
                amount,
                PAGE_BYTES as usize + 16,
            )
        {
            retention.persistence_attempt(false);
            return;
        }
        let reserve_dirty = || {
            admission
                .reserve(Some(cache), ResourceClass::DirtyCiphertext, amount)
                .map_err(Error::from)
        };
        let page_charge = copy
            .ciphertext
            .inner
            .reservation()
            .amount()
            .max(PAGE_BYTES as usize + 16);
        let dirty = self.reserve_owned_persistence(
            id,
            ResourceClass::DirtyCiphertext,
            amount,
            owned,
            reserve_dirty,
        );
        let Ok(dirty) = dirty else {
            retention.persistence_attempt(false);
            return;
        };
        // No await, new encryption, download, or mandatory resource reservation.
        let result = self
            .dependencies
            .writer
            .enqueue_reclaiming(copy, dirty, |amount| {
                // Ciphertext owns both the immutable page and its disk staging.
                // The one-page floor includes BOTH, even on tiny slabs. The page
                // term is the page's own Ciphertext charge, which for aligned
                // pages covers the padded body pool class. `amount` is the
                // staging request: only the padded header block when persist
                // writes the aligned body in place, or the full padded record
                // for the copy fallback, so the floor covers either path.
                let one_page = page_charge.saturating_add(amount);
                // Idle aligned buffers retain Ciphertext charges after completed
                // writes. Release that backing before headroom rejection or any
                // destructive queued-copy reclaim; this is one bounded pool.
                self.dependencies.writer.slabs().reclaim_idle();
                if !owned && !self.optional_staging_headroom(amount, one_page) {
                    return Err(Error::Overloaded);
                }
                self.reserve_owned_persistence(id, ResourceClass::Ciphertext, amount, owned, || {
                    admission
                        .reserve_completion(Some(cache), ResourceClass::Ciphertext, amount)
                        .map_err(Into::into)
                })
            });
        retention.persistence_attempt(result.is_ok());
    }

    /// Reclaim against the optional ceiling, not the larger hard quota. One
    /// bounded idle-only pass advances cursors for later independent interests.
    /// Never release queued copies or submitted/live owners to admit a write.
    pub(super) fn optional_staging_headroom(&self, amount: usize, one_page: usize) -> bool {
        let admission = &self.dependencies.admission;
        let class = ResourceClass::Ciphertext;
        let limit = admission.limit(class);
        let ceiling = racer_core::read::optional_ceiling(limit, one_page);
        if amount > ceiling {
            return false;
        }
        let available_used = ceiling - amount;
        if admission.used(class) <= available_used {
            return true;
        }
        admission.reclaim_buffers();
        let deficit = admission.used(class).saturating_sub(available_used);
        if deficit != 0 {
            // Over-share caches' idle bytes go first. Zero callbacks never
            // discard queued or submitted copies.
            self.dependencies.memory.reclaim(
                class,
                None,
                deficit,
                |cache| admission.excess(cache, class),
                |_, _| 0,
                |_, _| 0,
            );
            // Evicted payloads may have returned backing to the recycler rather
            // than releasing their charges. Drop those idle allocations too.
            admission.reclaim_buffers();
        }
        optional_headroom(limit, admission.used(class), amount, one_page)
    }

    /// At most one bounded queued-optional scan and one retry. Submitted copies
    /// remain fenced. Optional writes never evict another queued write to get
    /// their own dirty/staging reservation; they simply skip persistence.
    fn reserve_owned_persistence(
        &self,
        page: &PageId,
        class: ResourceClass,
        amount: usize,
        owned: bool,
        reserve: impl Fn() -> Result<flow_control::Charge<AdmissionPolicy>>,
    ) -> Result<flow_control::Charge<AdmissionPolicy>> {
        // Owned writeback may reclaim idle ciphertext, but never live readers.
        // Releasing readers must make their cached allocations usable for the
        // next disk staging reservation, not only a later mandatory acquisition.
        let reserve_idle = || {
            if owned && matches!(class, ResourceClass::Ciphertext) {
                let mut result = reserve();
                for _ in 0..2 {
                    if !matches!(result, Err(Error::Overloaded)) {
                        break;
                    }
                    let Some((cache, bytes)) = self.dependencies.admission.reclamation(
                        &page.version.object.cache,
                        class,
                        amount,
                    ) else {
                        break;
                    };
                    // Zero callbacks never discard queued writes to make a cache
                    // entry idle. Only unleased cache allocations may disappear,
                    // over-share caches first under global pressure.
                    self.dependencies.memory.reclaim(
                        class,
                        cache.as_ref(),
                        bytes,
                        |cache| self.dependencies.admission.excess(cache, class),
                        |_, _| 0,
                        |_, _| 0,
                    );
                    result = reserve();
                }
                result
            } else {
                reserve()
            }
        };
        let result = reserve_idle();
        if !owned || !matches!(result, Err(Error::Overloaded)) {
            return result;
        }
        let Some((cache, deficit)) =
            self.dependencies
                .admission
                .reclamation(&page.version.object.cache, class, amount)
        else {
            return result;
        };
        let (dirty, staging) = match class {
            ResourceClass::DirtyCiphertext => (deficit, 0),
            ResourceClass::Ciphertext => (0, deficit),
            _ => return result,
        };
        self.dependencies
            .writer
            .reclaim_optional_for_owned(page, cache.as_ref(), dirty, staging);
        reserve_idle()
    }

    pub(super) async fn publish(
        &self,
        result: PageResult,
        dirty: Option<flow_control::Charge<AdmissionPolicy>>,
        scope: &RequestScope,
    ) -> Result<()> {
        result.validate_metadata()?;
        // Passthrough pages reach their readers through the flight only; no
        // memory entry, descriptor catalog entry, or disk write retains them.
        if self.passthrough(&result.metadata.version.object.cache) {
            return Ok(());
        }
        if let Some(existing) = self
            .retained_metadata(&result.metadata.version, scope)
            .await?
            && !existing.compatible(&result.metadata.immutable())
        {
            return Err(Error::CorruptRecord);
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
            let _ = self
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
                });
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
        .map_err(Into::into)
}
#[cfg(test)]
mod tests {
    use super::*;
    use crate::model::CacheKey;
    use crate::model::ObjectId;
    use crate::model::ObjectVersion;
    use crate::model::StrongEtag;
    use crate::model::VersionMetadata;
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
