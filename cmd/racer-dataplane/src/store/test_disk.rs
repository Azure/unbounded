//! Test-only extent volume on the simulated disk, for index-level tests that
//! need disk pages without a full store writer.

use super::catalog::Index;
use crate::admission::AdmissionPolicy;
use crate::admission::ResourceClass;
use crate::model::PageId;
use crate::model::VersionMetadata;
use crate::runtime::Reactor;
use crate::runtime::RequestScope;
use page_alloc::Slab;
use page_alloc::volume::{SLOT_BYTES, Tier, UNIT_BYTES, Value, Volume, VolumeOptions};
use racer_control_wire::KeyId;
use std::future::Future;
use std::rc::Rc;
use std::task::{Context, Poll};
use uring_runtime::reactor::simulation::{Environment, Simulation};

/// A simulated slab with a mounted volume bound to one index.
pub(crate) struct TestDisk {
    pub(crate) reactor: Rc<Reactor>,
    pub(crate) admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
    pub(crate) slab: Rc<Slab<flow_control::Charge<AdmissionPolicy>>>,
    pub(crate) simulation: Simulation,
    _environment: Environment,
}

pub(crate) fn scope() -> RequestScope {
    RequestScope::new(
        crate::model::RequestId([9; 16]),
        std::time::Instant::now() + std::time::Duration::from_secs(30),
    )
    .unwrap()
}

impl TestDisk {
    /// Mount a fresh volume of `slots` slots and install it into `index`.
    pub(crate) fn mount(index: &Index, slots: u64) -> Self {
        let simulation = Simulation::new();
        let environment = simulation.enter();
        let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
            crate::test_support::cluster::config(false).limits,
        )));
        let reactor = Rc::new(Reactor::new(admission.clone()));
        reactor.init().unwrap();
        let slab = Rc::new(Slab::new(
            "/test-disk/slab".into(),
            slots * SLOT_BYTES,
            SLOT_BYTES,
            SLOT_BYTES as usize,
        ));
        let disk = Self {
            reactor,
            admission,
            slab,
            simulation,
            _environment: environment,
        };
        disk.remount(index);
        disk
    }

    /// Mount the slab's volume again (for example after a simulated crash)
    /// and install it into `index`.
    pub(crate) fn remount(&self, index: &Index) {
        let admission = self.admission.clone();
        let mut options = VolumeOptions::new(
            index.volume_limits(self.slab.capacity_bytes() / SLOT_BYTES),
            [7; 16],
            true,
            Rc::new(move |length| {
                admission
                    .reserve_completion(None, ResourceClass::Ciphertext, length)
                    .map_err(|_| page_alloc::Error::Busy)
            }),
        );
        // Small enough for tests to reach the dirty limit.
        options.dirty_bytes = 1 << 20;
        let scope = scope();
        let (volume, report) = self
            .drive(Volume::mount(
                &*self.reactor,
                self.slab.clone(),
                options,
                &scope,
            ))
            .unwrap();
        self.drive(index.install_volume(volume, self.reactor.clone(), &report, &scope))
            .unwrap();
    }

    /// Drop the volume and slab, lose every write that was not synced, and
    /// mount again into `index`.
    pub(crate) fn crash(mut self, index: &Index) -> Self {
        index.volume.borrow_mut().take();
        let capacity = self.slab.capacity_bytes();
        self.slab = Rc::new(Slab::new(
            "/test-disk/slab".into(),
            capacity,
            SLOT_BYTES,
            SLOT_BYTES as usize,
        ));
        self.simulation.disk().crash().unwrap();
        self.remount(index);
        self
    }

    /// Poll `future` and the reactor until it completes.
    pub(crate) fn drive<T>(&self, future: impl Future<Output = T>) -> T {
        let mut future = std::pin::pin!(future);
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        for _ in 0..1_000_000 {
            if let Poll::Ready(value) = future.as_mut().poll(&mut cx) {
                return value;
            }
            self.reactor.poll_budgeted(64).unwrap();
        }
        panic!("test disk operation did not complete");
    }

    /// Publish a one-unit placeholder record for `page`. The bytes are not a
    /// valid page record; this only populates the index.
    pub(crate) fn put(
        &self,
        index: &Index,
        page: &PageId,
        metadata: &VersionMetadata,
        key_id: KeyId,
        tier: Tier,
    ) -> Value {
        self.put_reserving(index, page, metadata, key_id, tier, UNIT_BYTES as u64)
    }

    /// [`TestDisk::put`] into a record of its own whole slot, which closes the
    /// slot so the volume can evict it.
    pub(crate) fn put_whole(
        &self,
        index: &Index,
        page: &PageId,
        metadata: &VersionMetadata,
        key_id: KeyId,
        tier: Tier,
    ) -> Value {
        self.put_reserving(index, page, metadata, key_id, tier, SLOT_BYTES)
    }

    /// Like [`TestDisk::put`], returning whether the page was published.
    pub(crate) fn try_put(
        &self,
        index: &Index,
        page: &PageId,
        metadata: &VersionMetadata,
        key_id: KeyId,
        tier: Tier,
    ) -> bool {
        self.publish(index, page, metadata, key_id, tier, UNIT_BYTES as u64)
    }

    fn put_reserving(
        &self,
        index: &Index,
        page: &PageId,
        metadata: &VersionMetadata,
        key_id: KeyId,
        tier: Tier,
        reserve: u64,
    ) -> Value {
        assert!(self.publish(index, page, metadata, key_id, tier, reserve));
        index.lookup(page).unwrap().unwrap()
    }

    fn publish(
        &self,
        index: &Index,
        page: &PageId,
        metadata: &VersionMetadata,
        key_id: KeyId,
        tier: Tier,
        reserve: u64,
    ) -> bool {
        let reservation = index.allocate_now(tier, reserve).unwrap();
        let generation = index.next_generation().unwrap();
        let charge = self
            .admission
            .reserve(None, ResourceClass::Ciphertext, UNIT_BYTES)
            .unwrap();
        let buffer = self.slab.allocate(UNIT_BYTES, charge).unwrap();
        let volume = index.volume().unwrap();
        let scope = scope();
        let _ = self
            .drive(volume.write(&*self.reactor, &reservation, buffer, &scope))
            .unwrap();
        index
            .publish_page(
                page,
                reservation,
                UNIT_BYTES as u32,
                generation,
                metadata,
                key_id,
            )
            .unwrap()
    }

    /// Run queued index maintenance, then commit the volume index.
    pub(crate) fn commit(&self, index: &Index) {
        let scope = scope();
        while index.maintenance_queued() {
            self.drive(index.maintain(&scope, false)).unwrap();
        }
        self.drive(index.commit(&self.reactor, &scope)).unwrap();
    }
}

impl Index {
    /// Remove a page only while its entry is still `value`, for tests that model
    /// an invalidation.
    pub(crate) fn remove_if_matches(&self, page: &PageId, value: &Value) {
        if let Some(key) = super::key::volume_key(page) {
            self.remove_if_generation(&key, value.generation);
        }
    }

    /// Byte offset and length of a page's record, for corruption tests.
    pub(crate) fn record_extent(&self, page: &PageId) -> Option<(u64, u64)> {
        self.disk_entry_now(page)
            .map(|(_, value)| (value.offset(), u64::from(value.length)))
    }
}
