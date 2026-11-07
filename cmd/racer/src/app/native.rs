//! One RDMA owner per node on its own thread, pinned to the crypto CPU paired
//! with the first I/O worker; every I/O worker gets a client
//! (`designs/racer-rdma-sessions.md`, "Threading").

use super::*;
use crate::runtime::HashMap;

#[derive(Default)]
pub(super) struct NativeNode {
    clients: Mutex<HashMap<WorkerId, rdma_verbs::Client>>,
    owner: Mutex<Option<rdma_verbs::OwnerPort>>,
    owner_worker: Mutex<Option<WorkerId>>,
    links: Arc<crate::rdma::LinkNode>,
    numa: Mutex<HashMap<WorkerId, NativePlacement>>,
}

/// NUMA locality of the crypto role that first-touches registered allocations.
#[derive(Clone, Copy)]
pub(super) struct NativePlacement {
    pub(super) numa_node: Option<usize>,
}

/// Page buffers the node-wide registered budget funds.
pub(super) fn buffer_count(limits: &Limits) -> usize {
    crate::rdma::buffer_count(limits.registered_bytes.get())
}

impl NativeNode {
    pub(super) fn place(&self, plan: &AffinityPlan) -> Result<()> {
        let mut numa = self.numa.lock().map_err(|_| Error::Unavailable)?;
        for pair in &plan.pairs {
            numa.insert(
                pair.worker,
                NativePlacement {
                    numa_node: pair.crypto.numa_node,
                },
            );
        }
        Ok(())
    }
    pub(super) fn numa(&self, worker: WorkerId) -> Result<Option<NativePlacement>> {
        Ok(self
            .numa
            .lock()
            .map_err(|_| Error::Unavailable)?
            .get(&worker)
            .copied())
    }
    /// Create the node owner and one client per worker. The first worker
    /// drives activation; the owner runs on its own thread (see
    /// [`Self::start_owner`]).
    pub(super) fn prepare(
        &self,
        workers: impl Iterator<Item = WorkerId>,
        limits: &Limits,
    ) -> Result<()> {
        // Insufficient optional native capacity uses HTTP without inventing quota.
        if buffer_count(limits) == 0 {
            return Ok(());
        }
        let workers: Vec<_> = workers.collect();
        let Some(&first) = workers.first() else {
            return Ok(());
        };
        let (clients, port) = rdma_verbs::node(workers.len())?;
        let mut map = self.clients.lock().map_err(|_| Error::Unavailable)?;
        map.extend(workers.into_iter().zip(clients));
        *self.owner.lock().map_err(|_| Error::Unavailable)? = Some(port);
        *self.owner_worker.lock().map_err(|_| Error::Unavailable)? = Some(first);
        Ok(())
    }
    /// This worker's client, once.
    pub(super) fn client(&self, worker: WorkerId) -> Result<Option<rdma_verbs::Client>> {
        Ok(self
            .clients
            .lock()
            .map_err(|_| Error::Unavailable)?
            .remove(&worker))
    }
    /// The worker that drives activation. The owner thread runs on its paired
    /// crypto CPU. Registered page pools do not follow it: each is first
    /// touched under a policy preferring its device's NUMA node.
    pub(super) fn owner_worker(&self) -> Option<WorkerId> {
        *self
            .owner_worker
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
    }
    pub(super) fn link_node(&self) -> Arc<crate::rdma::LinkNode> {
        self.links.clone()
    }
    /// Start the owner thread on `cpu`, once. `None` without RDMA funding.
    pub(super) fn start_owner(
        &self,
        cpu: usize,
        ctx: racer_io::WorkerCtx,
    ) -> Result<Option<OwnerThread>> {
        let Some(port) = self.owner.lock().map_err(|_| Error::Unavailable)?.take() else {
            return Ok(None);
        };
        OwnerThread::spawn(port, cpu, ctx).map(Some)
    }

    /// The owner, built on the calling thread, for tests that step it.
    #[cfg(any(test, feature = "dst"))]
    pub(super) fn take_owner(&self, ctx: racer_io::WorkerCtx) -> Option<rdma_verbs::Owner> {
        let port = self.owner.lock().unwrap().take()?;
        Some(rdma_verbs::Owner::new(port, ctx))
    }
}

/// The node's RDMA owner thread. Dropping it stops the owner and joins the
/// thread once the owner drained: a page buffer still held elsewhere keeps
/// it waiting (with a periodic diagnostic), so memory the NIC may write is
/// never freed early.
pub(super) struct OwnerThread {
    stop: Arc<rdma_verbs::OwnerStop>,

    handle: Option<std::thread::JoinHandle<Result<()>>>,
}

impl OwnerThread {
    /// Spawn `racer-rdma-owner`, pinned to `cpu`, and build the owner there.
    fn spawn(port: rdma_verbs::OwnerPort, cpu: usize, ctx: racer_io::WorkerCtx) -> Result<Self> {
        let (sender, receiver) = std::sync::mpsc::sync_channel(1);
        let handle = std::thread::Builder::new()
            .name("racer-rdma-owner".into())
            .spawn(move || -> Result<()> {
                let started = racer_io::pin::pin_cpu(cpu)
                    .map_err(Error::from)
                    .and_then(|()| {
                        rdma_verbs::OwnerRole::new(rdma_verbs::Owner::new(port, ctx))
                            .map_err(Error::from)
                    });
                let mut role = match started {
                    Ok((role, stop)) => {
                        let _ = sender.send(Ok(stop));
                        role
                    }
                    Err(error) => {
                        let _ = sender.send(Err(error));
                        return Err(error);
                    }
                };
                racer_io::exec::run(&mut role).map_err(Error::from)
            })
            .map_err(|_| Error::Io)?;
        match receiver.recv() {
            Ok(Ok(stop)) => Ok(Self {
                stop,
                handle: Some(handle),
            }),
            Ok(Err(error)) => {
                let _ = handle.join();
                Err(error)
            }
            Err(_) => {
                let _ = handle.join();
                Err(Error::Io)
            }
        }
    }

    /// Times the owner woke from a park.
    #[cfg(test)]
    pub(super) fn wakes(&self) -> u64 {
        self.stop.wakes()
    }

    /// Stop the owner and wait until it drained and its thread exited.
    pub(super) fn shutdown(mut self) -> Result<()> {
        self.finish()
    }

    fn finish(&mut self) -> Result<()> {
        self.stop.stop();
        match self.handle.take().map(std::thread::JoinHandle::join) {
            None => Ok(()),
            Some(Ok(result)) => result,
            Some(Err(_)) => Err(Error::Internal),
        }
    }
}

impl Drop for OwnerThread {
    fn drop(&mut self) {
        if let Err(error) = self.finish() {
            eprintln!("racer: stage=rdma-owner state=failed error={error:?}");
        }
    }
}

impl WorkerApplication {
    fn native_owner(&self) -> bool {
        self.links.is_some() && self.node.native.owner_worker() == Some(self.worker)
    }
    /// Close the node's links. Only the owner's worker closes: every worker sees
    /// the same inventory and publication, and the owner reactivates.
    pub(super) fn close_native(&mut self) {
        if self.native_owner()
            && let Some(links) = &self.links
        {
            links.close();
        }
    }
    /// Inventory withdrawal/GID changes invalidate in-progress activation before
    /// it can publish readiness. Existing links drain on the owner.
    pub(super) fn refresh_inventory(&mut self) -> Result<()> {
        if self.links.is_none() || self.node.inventory.generation() == self.inventory_generation {
            return Ok(());
        }
        let snapshot = self.node.inventory.snapshot()?;
        if snapshot.generation != self.inventory_generation {
            self.native_task.take();
            self.close_native();
            self.actual_rails.clear();
            self.discovered_nics = snapshot.nics;
            self.inventory_generation = snapshot.generation;
            self.native_retry = self.ctx().now();
        }
        Ok(())
    }
    /// The node's rails: one device per published rail, chosen near the owner.
    ///
    /// While this node's own rails are withdrawn (or advertised again but not
    /// yet republished), membership lists none for it, so the discovered
    /// rails stand in. Otherwise activation could never succeed, and only a
    /// successful activation advertises the rails again.
    pub(super) fn native_publication(&self) -> Result<Vec<racer_wire::control::RailMapping>> {
        let snapshot = self.snapshots.current()?;
        let member = snapshot.membership.member(self.keys.node())?;
        let published =
            if member.rails.is_empty() && (self.rdma_withdrawn || self.rdma_readvertised) {
                &self.discovered_nics
            } else {
                &member.rails
            };
        let owner = self.node.native.owner_worker();
        let numa = match owner {
            Some(owner) => self.node.native.numa(owner)?.and_then(|p| p.numa_node),
            None => None,
        };
        Ok(crate::rdma::select_worker(
            published,
            &self.discovered_nics,
            0,
            numa,
            publication_capacity(self.links.is_some()),
        ))
    }
    pub(super) fn poll_native(&mut self, cx: &mut Context<'_>) -> Result<()> {
        if self.stopping {
            self.native_task.take();
            return Ok(());
        }
        let Some(links) = self.links.clone() else {
            return Ok(());
        };
        self.refresh_inventory()?;
        if self.rdma_readvertised
            && let Ok(snapshot) = self.snapshots.current()
            && snapshot
                .membership
                .member(self.keys.node())
                .is_ok_and(|member| !member.rails.is_empty())
        {
            // Membership lists the advertised rails again.
            self.rdma_readvertised = false;
        }
        if let Some(task) = self.native_task.as_mut()
            && let Poll::Ready(result) = task.as_mut().poll(cx)
        {
            self.native_task = None;
            // An activation that selected no rail is a failure too: counted,
            // logged, and retried, never a silent idle round.
            let result = match result {
                Ok(rails) if rails.is_empty() && !self.attaching_landings => {
                    eprintln!("racer: stage=rdma-activation step=settle reason=no_rail_selected");
                    Err(Error::Unavailable)
                }
                result => result,
            };
            self.attaching_landings = false;
            match &result {
                Err(error) => {
                    links.close();
                    self.native_failures = self.native_failures.saturating_add(1);
                    self.native_activation_failed(*error);
                    if self.rdma_lost.is_some() {
                        self.reactivation_attempt_failed()?;
                    }
                }
                Ok(rails) if !rails.is_empty() => {
                    self.native_failures = 0;
                    self.native_activation_succeeded();
                    if let Some(lost) = self.rdma_lost.take() {
                        self.telemetry
                            .metrics
                            .record(crate::telemetry::Event::RdmaDeviceReactivation, 1);
                        eprintln!(
                            "racer: stage=rdma-device state=reactivated rails={} lost={}",
                            rails.len(),
                            lost.len()
                        );
                    }
                }
                Ok(_) => {}
            }
            // After a device loss, attempts are bounded by
            // `REACTIVATION_ATTEMPTS`, so they retry at the probe interval
            // rather than backing off: a process that cannot reopen the
            // device then restarts within seconds, not minutes.
            let delay = if self.rdma_lost.is_some() {
                RAIL_PROBE
            } else {
                activation_retry_delay(self.native_failures)
            };
            self.native_retry = self.ctx().now() + delay;
        }
        self.actual_rails = links.active();
        if self.native_owner() {
            self.watch_devices(&links);
        }
        if self.native_owner() {
            let metrics = &self.telemetry.metrics;
            metrics.set_gauge(
                crate::telemetry::Gauge::RdmaActive,
                u64::from(!self.actual_rails.is_empty()),
            );
            metrics.set_gauge(
                crate::telemetry::Gauge::RdmaFreeBuffers,
                links.free_buffers() as u64,
            );
            metrics.set_gauge(
                crate::telemetry::Gauge::RdmaGrantWaiters,
                links.grant_waiters() as u64,
            );
            links.sync_link_metrics();
        }
        // The GPU helper started lazily after these rails activated: attach
        // its landing buffers to them now. Only the owner registers memory.
        if self.native_owner()
            && self.native_task.is_none()
            && !self.actual_rails.is_empty()
            && links.node().take_landing_pending()
        {
            let rails = self.actual_rails.clone();
            self.attaching_landings = true;
            let turn = scope(&self.ctx().clock, Duration::from_secs(5))?;
            self.native_task = Some(Box::pin(async move {
                links.attach_landings(&rails, &turn).await;
                Ok(rails)
            }));
            cx.waker().wake_by_ref();
            return Ok(());
        }
        if !self.native_owner()
            || self.native_task.is_some()
            || !self.actual_rails.is_empty()
            || self.ctx().now() < self.native_retry
        {
            return Ok(());
        }
        if self.rdma_lost.is_some() && !self.lost_devices_back(&links) {
            if self.rdma_waiting.as_deref() == Some("device_undiscovered") {
                self.reactivation_attempt_failed()?;
            }
            self.native_retry = self.ctx().now() + RAIL_PROBE;
            return Ok(());
        }
        let publication = match self.native_publication() {
            // Not a member yet: a new node joins only after it serves.
            // Activate once a publication lists it.
            Err(Error::IncompatibleMembership) => {
                self.note_waiting(Some("membership_missing".into()));
                return Ok(());
            }
            Err(error) => {
                self.note_waiting(Some(format!("publication:{error:?}")));
                return Err(error);
            }
            Ok(publication) => publication,
        };
        if publication.is_empty() {
            if self.rdma_lost.is_some() {
                self.native_retry = self.ctx().now() + RAIL_PROBE;
            }
            self.note_waiting(Some(format!(
                "publication_empty:discovered={}",
                self.discovered_nics.len()
            )));
            return Ok(());
        }
        self.note_waiting(None);
        eprintln!(
            "racer: stage=rdma-activation state=starting rails={:?}",
            publication
                .iter()
                .map(|m| m.device.as_str())
                .collect::<Vec<_>>()
        );
        let admission = self.runtime.admission.clone();
        let registered = admission.limit(crate::admission::ResourceClass::Registered);
        let turn = scope(
            &self.ctx().clock,
            activation_turn(registered, publication.len()),
        )?;
        self.native_task = Some(Box::pin(async move {
            links
                .activate(publication, &admission, registered, &turn)
                .await
        }));
        cx.waker().wake_by_ref();
        Ok(())
    }
    pub(super) async fn activate_native(&mut self, startup: &RequestScope) -> Result<()> {
        self.refresh_inventory()?;
        let Some(links) = self.links.clone() else {
            return Ok(());
        };
        if !self.native_owner() {
            // Rails are node-wide; other workers read the owner's activation
            // when it settles (see `poll_native`) and never wait for it here.
            self.actual_rails = links.active();
            return Ok(());
        }
        let publication = match self.native_publication() {
            // Not a member yet: start serving over TCP and let
            // `poll_native` activate once a publication lists this node.
            Err(Error::IncompatibleMembership) => return Ok(()),
            result => result?,
        };
        if publication.is_empty() {
            return Ok(());
        }
        match links
            .activate(
                publication,
                &self.runtime.admission,
                self.runtime
                    .admission
                    .limit(crate::admission::ResourceClass::Registered),
                startup,
            )
            .await
        {
            Ok(actual) => {
                self.actual_rails = actual;
            }
            Err(Error::Canceled | Error::DeadlineExceeded) => {
                startup.check()?;
            }
            Err(error) => {
                links.close();
                self.native_activation_failed(error);
            }
        }
        Ok(())
    }
}

/// Time one native activation of `devices` rails may take: 5 s, plus 1 s per
/// GiB of registered page buffers, because registering memory that is not
/// huge-page backed costs up to about 0.75 s per GiB, plus 2 s per device for
/// its implicit on-demand paging region, which takes about 1.5 s per mlx5
/// device. Those regions are created in parallel, but the per-device term
/// keeps activation within its turn even when the kernel serializes them. A
/// fixed 5 s turn silently failed every retry on large pools, and a turn
/// without the per-device term failed every retry on 8 rails with 8 GiB.
pub(crate) fn activation_turn(registered_bytes: usize, devices: usize) -> Duration {
    const GIB: usize = 1 << 30;
    const PER_DEVICE: u64 = 2;
    Duration::from_secs(
        (5 + registered_bytes.div_ceil(GIB) as u64)
            .saturating_add(PER_DEVICE.saturating_mul(devices as u64)),
    )
}

/// How long to wait before the next activation after `failures` consecutive
/// failed ones: 1 s, doubling per failure up to 60 s. Each attempt opens
/// devices and registers memory from scratch, so a persistently failing
/// activation must not retry in a tight loop.
pub(crate) fn activation_retry_delay(failures: u32) -> Duration {
    const MAX: Duration = Duration::from_secs(60);
    let shift = failures.saturating_sub(1).min(6);
    Duration::from_secs(1u64 << shift).min(MAX)
}

impl WorkerApplication {
    /// Count and log a failed activation, and stop advertising this node's
    /// rails so peers stop planning RDMA to it. Activation keeps retrying.
    fn native_activation_failed(&mut self, error: Error) {
        self.telemetry
            .metrics
            .record(crate::telemetry::Event::RdmaActivationFailure, 1);
        if self.rdma_withdrawn {
            // Retries log too, on the 1st, 2nd, 4th, 8th, ... consecutive
            // failure, so a reactivation that keeps failing stays visible
            // without one line per retry.
            if self.native_failures.is_power_of_two() {
                eprintln!(
                    "racer: stage=rdma-activation state=failed error={error:?} failures={}; RDMA rails stay withdrawn, retrying",
                    self.native_failures
                );
            }
        } else {
            eprintln!(
                "racer: stage=rdma-activation state=failed error={error:?}; withdrawing RDMA rails and retrying"
            );
            if let Some((report, shares)) = &self.rdma_report {
                report(*shares, Vec::new());
            }
            self.rdma_withdrawn = true;
        }
    }

    /// Advertise the discovered rails again after a withdrawal.
    fn native_activation_succeeded(&mut self) {
        if std::mem::take(&mut self.rdma_withdrawn) {
            eprintln!("racer: stage=rdma-activation state=active; advertising RDMA rails again");
            self.rdma_readvertised = true;
            if let Some((report, shares)) = &self.rdma_report {
                report(*shares, self.discovered_nics.clone());
            }
        }
    }
}

/// How often the owner worker checks sysfs that every active rail's device
/// is still there, and, after a loss, whether it came back.
pub(crate) const RAIL_PROBE: Duration = Duration::from_secs(2);

/// How long the device names at a lost device's address must hold still
/// before reactivation lists devices, so udev's rename of a replugged VF
/// lands first.
pub(crate) const RAIL_SETTLE: Duration = Duration::from_secs(5);

/// Reactivation attempts, after a lost device is back and settled, before
/// the process restarts to rebuild its verbs device list.
pub(crate) const REACTIVATION_ATTEMPTS: u32 = 6;

impl WorkerApplication {
    /// Close the round when an active rail's device is gone: the owner saw
    /// it removed, or sysfs no longer lists it (or lists another device at
    /// its name). Only PCI-keyed rails are probed in sysfs; name-keyed ones
    /// (no PCI address known) rely on the owner's report.
    fn watch_devices(&mut self, links: &crate::rdma::Links) {
        let losses = links.device_losses();
        let reported = losses != self.rdma_losses_seen;
        self.rdma_losses_seen = losses;
        if self.rdma_lost.is_some() || self.actual_rails.is_empty() {
            return;
        }
        let now = self.ctx().now();
        let mut missing = Vec::new();
        if !reported && now >= self.rail_probe {
            self.rail_probe = now + RAIL_PROBE;
            missing = self
                .actual_rails
                .iter()
                .zip(self.rail_keys())
                .filter(|(_, (device, key))| {
                    key.starts_with("pci:")
                        && crate::rdma::rail_presence(&self.rdma_sysfs, device, key)
                            != crate::rdma::RailPresence::Present
                })
                .map(|(mapping, _)| mapping.rail)
                .collect();
        }
        if reported || !missing.is_empty() {
            // The owner learns of losses only sysfs shows, so the round does
            // not wait for pages no NIC can reach any more.
            for rail in missing {
                links.report_lost(rail);
            }
            self.native_device_lost(links);
        }
    }

    /// `(device, hardware key)` of each active rail.
    fn rail_keys(&self) -> Vec<(String, String)> {
        self.actual_rails
            .iter()
            .map(|m| {
                let key = self
                    .node
                    .inventory
                    .device_key(&m.device, m.port)
                    .unwrap_or_else(|| m.device.clone());
                (m.device.clone(), key)
            })
            .collect()
    }

    /// Fall back to HTTP at once and wait for the device to come back:
    /// close the round (readers stop planning RDMA as soon as the rails
    /// deactivate, and every link's ops fail), withdraw the rails from
    /// peers, and let activation retry once the device is back.
    fn native_device_lost(&mut self, links: &crate::rdma::Links) {
        let lost = self.rail_keys();
        eprintln!(
            "racer: stage=rdma-device state=lost devices={:?}; using HTTP until the device is back",
            lost.iter().map(|(device, _)| device).collect::<Vec<_>>()
        );
        self.telemetry
            .metrics
            .record(crate::telemetry::Event::RdmaDeviceLoss, 1);
        self.native_task.take();
        links.close();
        self.actual_rails.clear();
        if !self.rdma_withdrawn {
            if let Some((report, shares)) = &self.rdma_report {
                report(*shares, Vec::new());
            }
            self.rdma_withdrawn = true;
        }
        self.rdma_lost = Some(lost);
        self.rdma_waiting = None;
        self.rdma_settle = None;
        self.rdma_attempts = 0;
        self.rdma_char_refreshed = false;
        self.native_failures = 0;
        self.native_retry = self.ctx().now() + RAIL_PROBE;
    }

    /// Whether every lost device is back and the old round drained, so a
    /// new activation can start. A device back under a new name (a VF
    /// replug) refreshes the inventory first, so activation finds it.
    ///
    /// Each change in what it waits for is logged once and shown by the
    /// `racer_rdma_reactivation_waiting_*` gauges, and a failed inventory
    /// refresh is logged and retried, so a stalled reactivation is visible.
    fn lost_devices_back(&mut self, links: &crate::rdma::Links) -> bool {
        let waiting = self.reactivation_waiting(links);
        let metrics = &self.telemetry.metrics;
        metrics.set_gauge(
            crate::telemetry::Gauge::RdmaWaitingDevice,
            u64::from(matches!(&waiting, Some(w) if w.starts_with("device"))),
        );
        metrics.set_gauge(
            crate::telemetry::Gauge::RdmaWaitingDrain,
            u64::from(matches!(&waiting, Some(w) if w.starts_with("drain"))),
        );
        let clear = waiting.is_none();
        if !clear {
            self.note_waiting(waiting);
        }
        clear
    }

    /// Count a reactivation attempt that found the device back but could not
    /// use it. After [`REACTIVATION_ATTEMPTS`] of them the process's verbs
    /// device list is taken to be stale (see [`Self::reactivation_waiting`]),
    /// which only a restart clears: fail the worker, so the process exits and
    /// its supervisor restarts it with RDMA. HTTP serves meanwhile.
    fn reactivation_attempt_failed(&mut self) -> Result<()> {
        self.rdma_attempts = self.rdma_attempts.saturating_add(1);
        if self.rdma_attempts < REACTIVATION_ATTEMPTS {
            return Ok(());
        }
        eprintln!(
            "racer: stage=rdma-device state=restarting reason=device_unusable attempts={}; the device is back but this process cannot open it",
            self.rdma_attempts
        );
        Err(Error::Unavailable)
    }

    /// Log once per change why RDMA is not activating after a device loss.
    fn note_waiting(&mut self, waiting: Option<String>) {
        if self.rdma_lost.is_none() || waiting == self.rdma_waiting {
            return;
        }
        match &waiting {
            Some(reason) => eprintln!(
                "racer: stage=rdma-device state=waiting reason={reason}; reactivating once it clears"
            ),
            None => eprintln!("racer: stage=rdma-device state=ready; activating"),
        }
        self.rdma_waiting = waiting;
    }

    /// What reactivation after a device loss waits for, or `None` once it
    /// may start: `device_missing:<name>`, `device_inactive:<name>`,
    /// `device_settling`, `drain_<blocker>`
    /// (see [`rdma_verbs::DrainBlocker`]), `inventory_refresh:<error>`, or
    /// `device_undiscovered`.
    ///
    /// libibverbs keeps a device it listed once under the name it had then,
    /// even after udev renames it (an Azure VF comes back as `mana_0` and
    /// becomes `roceP30832p0s0` moments later), and it cannot be opened
    /// under that stale name for the life of the process. So nothing lists
    /// devices until the names at every lost device's address held still
    /// for [`RAIL_SETTLE`].
    fn reactivation_waiting(&mut self, links: &crate::rdma::Links) -> Option<String> {
        let lost = self.rdma_lost.clone()?;
        let mut renamed = false;
        let mut names = Vec::with_capacity(lost.len());
        for (device, key) in &lost {
            match crate::rdma::rail_presence(&self.rdma_sysfs, device, key) {
                crate::rdma::RailPresence::Present => {}
                crate::rdma::RailPresence::Renamed => renamed = true,
                // A name-keyed rail cannot be found by address: try anyway.
                crate::rdma::RailPresence::Missing if !key.starts_with("pci:") => {}
                crate::rdma::RailPresence::Missing => {
                    self.rdma_settle = None;
                    return Some(format!("device_missing:{device}"));
                }
            }
            let at = crate::rdma::rail_devices(&self.rdma_sysfs, key);
            if !at.is_empty()
                && !at
                    .iter()
                    .any(|name| crate::rdma::device_active(&self.rdma_sysfs, name))
            {
                // Back, but no port is up yet: not an attempt.
                self.rdma_settle = None;
                return Some(format!("device_inactive:{device}"));
            }
            names.push(at);
        }
        let now = self.ctx().now();
        match &self.rdma_settle {
            Some((_, seen)) if *seen == names => {}
            _ => self.rdma_settle = Some((now, names.clone())),
        }
        if self
            .rdma_settle
            .as_ref()
            .is_some_and(|(since, _)| now < *since + self.rail_settle)
        {
            return Some("device_settling".into());
        }
        if !links.drained() {
            let blocker = links
                .drain_blocker()
                .map_or("pending", rdma_verbs::DrainBlocker::as_str);
            return Some(format!("drain_{blocker}"));
        }
        if !self.rdma_char_refreshed {
            self.rdma_char_refreshed = true;
            if let Some((sysfs, dir)) = &self.rdma_char_devices {
                match crate::rdma::refresh_char_devices(sysfs, dir) {
                    Ok(count) => eprintln!(
                        "racer: stage=rdma-device state=char-devices-refreshed count={count} dir={}",
                        dir.display()
                    ),
                    // Reopening may still work; a restart covers it if not.
                    Err(error) => eprintln!(
                        "racer: stage=rdma-device state=char-devices-failed dir={} error={error}",
                        dir.display()
                    ),
                }
            }
        }
        // A device back under its old name still needs a refresh when the
        // inventory dropped it meanwhile: a re-enrollment refreshes the
        // shared inventory while the device is gone, and nothing else
        // rediscovers it, so activation would find no rail forever.
        let undiscovered = |nics: &[racer_wire::control::RailMapping]| {
            lost.iter().zip(&names).any(|((device, _), names)| {
                // A listed name sysfs no longer shows is a stale one.
                !nics.iter().any(|n| {
                    if names.is_empty() {
                        &n.device == device
                    } else {
                        names.contains(&n.device)
                    }
                })
            })
        };
        if renamed || undiscovered(&self.discovered_nics) {
            // Opens the devices: only once they are all back.
            if let Err(error) = self
                .node
                .inventory
                .refresh()
                .and_then(|_| self.refresh_inventory())
            {
                return Some(format!("inventory_refresh:{error:?}"));
            }
            if undiscovered(&self.discovered_nics) {
                return Some("device_undiscovered".into());
            }
        }
        None
    }
}

/// Rails discovery may select. Every rail is eligible when links exist;
/// `funded_devices` in activation applies the node-wide buffer budget.
fn publication_capacity(links: bool) -> usize {
    if links { usize::MAX } else { 0 }
}

/// Whether active rails still match the publication activation would fund.
/// Activation truncates the publication to the budget-funded prefix, so the
/// comparison applies the same truncation before matching.
pub(super) fn rails_compatible(
    mut published: Vec<racer_wire::control::RailMapping>,
    actual: &[racer_wire::control::RailMapping],
    registered_bytes: usize,
) -> bool {
    crate::rdma::links::fund_publication(&mut published, registered_bytes);
    published.len() == actual.len()
        && actual.iter().all(|actual| {
            published.iter().any(|published| {
                published.rail == actual.rail
                    && published.device == actual.device
                    && published.port == actual.port
                    && published.gid.is_none_or(|gid| actual.gid == Some(gid))
                    && published
                        .numa_node
                        .is_none_or(|numa| actual.numa_node == Some(numa))
            })
        })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn rails_compatible_applies_registered_budget() {
        use crate::rdma::links::BUFFER_LEN;
        let nic = |device: &str, rail| racer_wire::control::RailMapping {
            device: device.into(),
            port: 1,
            rail: RailId(rail),
            gid: Some([1; 16]),
            numa_node: Some(0),
        };
        let eligible = vec![nic("a", 1), nic("b", 2), nic("c", 3)];
        // Budget funds two of three eligible rails: an unchanged refresh keeps links.
        let funded = vec![nic("a", 1), nic("b", 2)];
        assert!(rails_compatible(eligible.clone(), &funded, 2 * BUFFER_LEN));
        // Full budget: all three rails must be active.
        assert!(rails_compatible(
            eligible.clone(),
            &eligible,
            3 * BUFFER_LEN
        ));
        assert!(!rails_compatible(eligible.clone(), &funded, 3 * BUFFER_LEN));
        // A real change in a funded rail re-activates.
        let mut moved = eligible.clone();
        moved[1].device = "z".into();
        assert!(!rails_compatible(moved, &funded, 2 * BUFFER_LEN));
        let mut regid = eligible.clone();
        regid[0].gid = Some([2; 16]);
        assert!(!rails_compatible(regid, &funded, 2 * BUFFER_LEN));
        // A rail removed from the snapshot re-activates.
        assert!(!rails_compatible(
            vec![nic("a", 1)],
            &funded,
            2 * BUFFER_LEN
        ));
        // Budget shrinking below the active set re-activates.
        assert!(!rails_compatible(eligible.clone(), &funded, BUFFER_LEN));
        // Budget funding nothing never matches active rails.
        assert!(!rails_compatible(eligible.clone(), &funded, BUFFER_LEN - 1));
        // Changes past the funded prefix do not tear down links.
        let mut tail = eligible;
        tail[2].device = "y".into();
        assert!(rails_compatible(tail, &funded, 2 * BUFFER_LEN));
    }
    use crate::admission::ResourceClass;
    use crate::security;
    use crate::security::CryptoClient;
    use racer_wire::control::MembershipVersion;
    use racer_wire::control::Publication;
    use racer_wire::control::PublicationSequence;
    use racer_wire::control::RailId;
    use std::num::NonZeroUsize;

    #[test]
    fn publication_selects_every_rail_and_budget_limits_activation() {
        use crate::rdma::links::{BUFFER_LEN, funded_devices};
        let nic = |device: &str, rail| racer_wire::control::RailMapping {
            device: device.into(),
            port: 1,
            rail: RailId(rail),
            gid: Some([1; 16]),
            numa_node: Some(0),
        };
        let nics = vec![nic("a", 1), nic("b", 2), nic("c", 3)];
        let select = |links| {
            crate::rdma::select_worker(&nics, &nics, 0, Some(0), publication_capacity(links))
        };
        assert_eq!(select(true).len(), 3);
        assert!(select(false).is_empty());
        // Budget funds every rail: all activate.
        // One receive buffer per device suffices.
        assert_eq!(funded_devices(3 * BUFFER_LEN, 3), 3);
        assert_eq!(funded_devices(usize::MAX, 3), 3);
        // Budget funds only some rails: only those activate.
        assert_eq!(funded_devices(3 * BUFFER_LEN - 1, 3), 2);
        assert_eq!(funded_devices(BUFFER_LEN, 3), 1);
        assert_eq!(funded_devices(BUFFER_LEN - 1, 3), 0);
    }

    #[test]
    fn shared_inventory_recovers_new_hardware_and_revokes_removed_or_changed_ports() {
        use rdma_verbs::simulation::Device;
        use rdma_verbs::simulation::Simulation;
        let sim = Simulation::new()
            .with_devices(vec![
                Device::new("missing", [1; 16]),
                Device::new("new", [2; 16]),
            ])
            .unwrap();
        let _devices = sim.enter();
        let env = racer_io::Env::host();
        let app = configured();
        let (mut worker, engine) = worker(&env, &app, true, true);
        let mut service = test_owner(&app, engine);
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        let mut publication = racer_wire::control::Publication {
            pools: Vec::new(),
            schema_version: racer_wire::control::SCHEMA_VERSION,
            cluster: app.config.cluster.clone(),
            sequence: PublicationSequence(2),
            membership_version: MembershipVersion(2),
            members: vec![racer_wire::control::Member {
                node: app.config.node.clone(),
                shares: std::num::NonZeroU32::new(1).unwrap(),
                peer_endpoint: "127.0.0.1:7443".into(),
                rails: vec![racer_wire::control::RailMapping {
                    device: "new".into(),
                    port: 1,
                    gid: None,
                    rail: RailId(7),
                    numa_node: None,
                }],
                site: "site1".into(),
            }],
            caches: vec![],
            page_size: racer_wire::object::PageSize::DEFAULT,
        }
        .with_default_pool();
        worker.snapshots.apply(publication.clone()).unwrap();
        app.node.inventory.update(vec![]).unwrap();
        worker.poll_native(&mut cx).unwrap();
        assert!(worker.native_publication().unwrap().is_empty());
        let fresh = crate::rdma::inventory();
        app.node.inventory.update(fresh.clone()).unwrap();
        for _ in 0..20 {
            worker.poll_native(&mut cx).unwrap();
            service
                .poll_budgeted(&mut Context::from_waker(futures::task::noop_waker_ref()), 8)
                .unwrap();
        }
        assert_eq!(worker.actual_rails[0].device, "new");
        assert!(worker.links.as_ref().unwrap().rail_ready(RailId(7)));
        // No membership change is needed for local discovery withdrawal to revoke.
        app.node.inventory.update(vec![]).unwrap();
        worker.refresh_inventory().unwrap();
        assert!(!worker.links.as_ref().unwrap().rail_ready(RailId(7)));
        service
            .poll_budgeted(
                &mut Context::from_waker(futures::task::noop_waker_ref()),
                256,
            )
            .unwrap();
        assert_eq!(worker.runtime.admission.used(ResourceClass::Registered), 0);
        app.node.inventory.update(fresh.clone()).unwrap();
        for _ in 0..20 {
            worker.poll_native(&mut cx).unwrap();
            service
                .poll_budgeted(&mut Context::from_waker(futures::task::noop_waker_ref()), 8)
                .unwrap();
        }
        assert!(worker.links.as_ref().unwrap().rail_ready(RailId(7)));
        let mut changed = fresh;
        changed.iter_mut().find(|n| n.device == "new").unwrap().gid = Some([3; 16]);
        app.node.inventory.update(changed).unwrap();
        worker.refresh_inventory().unwrap();
        assert!(!worker.links.as_ref().unwrap().rail_ready(RailId(7)));
        service
            .poll_budgeted(
                &mut Context::from_waker(futures::task::noop_waker_ref()),
                256,
            )
            .unwrap();
        assert_eq!(worker.runtime.admission.used(ResourceClass::Registered), 0);
        // Published GID must still match refreshed discovery, never bypass it.
        publication.sequence.0 += 1;
        publication.membership_version.0 += 1;
        publication.members[0].rails[0].gid = Some([2; 16]);
        worker.snapshots.apply(publication).unwrap();
        assert!(worker.native_publication().unwrap().is_empty());
        drop(service);
        assert_eq!(sim.live_resources(), 0);
    }

    /// The node owner stepped by hand on the test thread, standing in for
    /// its own thread.
    struct TestOwner(rdma_verbs::Owner);

    impl TestOwner {
        fn poll_budgeted(&mut self, _: &mut Context<'_>, budget: usize) -> Result<()> {
            Ok(self.0.poll_budgeted(budget)?)
        }
    }

    fn test_owner(app: &Application, engine: CryptoRuntime) -> TestOwner {
        TestOwner(app.node.native.take_owner(engine.ctx).unwrap())
    }

    fn default_config(rdma: bool) -> Config {
        Config::from_lookup(|name| {
            Ok(match name {
                "RACER_CLUSTER_ID" => Some("00000000-0000-4000-8000-000000000001".into()),
                "RACER_CONTROL_ENDPOINT" => Some("https://control.example".into()),
                "RACER_ENABLE_RDMA" => Some(rdma.to_string()),
                _ => None,
            })
        })
        .unwrap()
    }

    #[test]
    fn operator_profile_reaches_runtime_admission_and_progress_floors() {
        // Parse the actual deployed ConfigMap defaults, not a second test profile.
        let manifest = include_str!("../../../../deploy/racer/dataplane-config.yaml.tmpl");
        let values: std::collections::HashMap<_, _> = manifest
            .lines()
            .filter_map(|line| line.trim().split_once(": \""))
            .filter(|(key, _)| key.starts_with("RACER_"))
            .map(|(key, value)| (key, value.trim_end_matches('"')))
            .collect();
        assert_eq!(
            values.len(),
            10 + usize::from(values.contains_key("RACER_MAX_THREADS"))
        );
        assert_eq!(values.get("RACER_CONTROL_SEATS"), Some(&"auto"));
        // Through the map parser, so a stale or misspelled key fails here.
        let mut environment: std::collections::BTreeMap<String, String> = values
            .iter()
            .map(|(k, v)| ((*k).to_owned(), (*v).to_owned()))
            .collect();
        environment.insert(
            "RACER_CLUSTER_ID".into(),
            "00000000-0000-4000-8000-000000000001".into(),
        );
        environment.insert(
            "RACER_CONTROL_ENDPOINT".into(),
            "https://control.example".into(),
        );
        let mut config = Config::from_map(&environment).unwrap();
        assert_eq!(
            config.enable_rdma,
            cfg!(feature = "rdma") && rdma_verbs::inventory().is_ok_and(|ports| !ports.is_empty())
        );
        // The following assertions exercise the HTTP budget profile independently
        // of whether the host has the optional native adapter installed.
        config.enable_rdma = false;
        assert_eq!(
            config.max_threads,
            values
                .get("RACER_MAX_THREADS")
                .map(|value| value.parse::<usize>().unwrap())
                .unwrap_or(crate::config::DEFAULT_MAX_THREADS)
        );
        assert_eq!(config.slab_bytes, 4 * 1024 * 1024 * 1024);
        assert_eq!(config.segment_bytes, 1024 * 1024 * 1024);
        // Byte budgets stay unset so the runtime scales them per I/O worker.
        for key in [
            "RACER_PLAINTEXT_BYTES",
            "RACER_CIPHERTEXT_BYTES",
            "RACER_DIRTY_BYTES",
            "RACER_REGISTERED_BYTES",
            "RACER_REQUEST_CONTEXT_BYTES",
        ] {
            assert!(!values.contains_key(key), "{key}");
        }
        let per_worker = config.limits.clone();
        let mut plan = four_pair_plan(&config);
        config.scale_limits(plan.pairs.len()).unwrap();
        let limits = size_workers(&config.limits, &mut plan, config.enable_rdma).unwrap();
        assert_eq!(plan.pairs.len(), 5);
        let admission = flow_control::Quotas::new(AdmissionPolicy::new(limits.clone()));
        let page = crate::model::PAGE_BYTES as usize;
        // Progress floors hold one page more than the range window.
        let pages = config.limits.range_window_pages.get() + 1;
        for (class, floor) in [
            (ResourceClass::Plaintext, pages * page),
            (
                ResourceClass::Ciphertext,
                pages * (page + 16) + crate::store::MAX_HEADER_BYTES,
            ),
            (ResourceClass::DirtyCiphertext, page + 16),
            (
                ResourceClass::RequestContext,
                crate::peer::protocol::MIN_REQUEST_CONTEXT_BYTES
                    + 4 * limits.header_bytes.get().max(crate::model::MAX_FIELD_BYTES),
            ),
        ] {
            let reservation = admission.reserve(None, class, floor).unwrap();
            assert_eq!(admission.used(class), floor);
            drop(reservation);
            assert_eq!(admission.used(class), 0);
        }
        assert_eq!(limits.plaintext_bytes, per_worker.plaintext_bytes);
        assert_eq!(limits.ciphertext_bytes, per_worker.ciphertext_bytes);
        assert_eq!(limits.dirty_bytes, per_worker.dirty_bytes);
        assert_eq!(
            limits.request_context_bytes,
            per_worker.request_context_bytes
        );
        assert_eq!(admission.used(ResourceClass::Registered), 0);
        // Reducing thread cap or quotas keeps local groups and re-partitions the
        // node budget. An impossible byte floor must fail rather than deadlock.
        let mut too_small = config.limits.clone();
        too_small.plaintext_bytes = NonZeroUsize::new(pages * page - 1).unwrap();
        assert!(size_workers(&too_small, &mut plan, false).is_err());
    }

    fn operator_profile_config() -> Config {
        let manifest = include_str!("../../../../deploy/racer/dataplane-config.yaml.tmpl");
        let values: std::collections::HashMap<_, _> = manifest
            .lines()
            .filter_map(|line| line.trim().split_once(": \""))
            .filter(|(key, _)| key.starts_with("RACER_"))
            .map(|(key, value)| (key, value.trim_end_matches('"')))
            .collect();
        Config::from_lookup(|name| {
            Ok(values
                .get(name)
                .map(|v| (*v).to_owned())
                .or_else(|| match name {
                    "RACER_CLUSTER_ID" => Some("00000000-0000-4000-8000-000000000001".into()),
                    "RACER_CONTROL_ENDPOINT" => Some("https://control.example".into()),
                    _ => None,
                }))
        })
        .unwrap()
    }

    fn core_plan(config: &Config, cores: usize) -> AffinityPlan {
        use crate::affinity::CpuLocation;
        use crate::affinity::EffectiveTopology;
        AffinityPlan::from_topology(
            config,
            EffectiveTopology {
                cpus: (0..cores)
                    .map(|cpu| CpuLocation {
                        cpu,
                        package: 0,
                        core: cpu,
                        numa_node: None,
                    })
                    .collect(),
                quota: None,
                nics: vec![],
            },
            &[],
        )
        .unwrap()
    }

    #[test]
    fn operator_profile_funds_every_planned_worker_on_large_nodes() {
        // A Standard_D64ds_v6 plans 21 I/O workers; fixed 256 MiB node-wide
        // budgets could fund only five and crash-looped startup.
        for rdma in [false, true] {
            for cores in [2, 4, 8, 16, 32, 64, 96, 128, 192] {
                let mut config = operator_profile_config();
                config.enable_rdma = rdma;
                let mut plan = core_plan(&config, cores);
                let planned = plan.pairs.len();
                assert!(
                    cores < 64 || planned > 10,
                    "cores={cores} planned={planned}"
                );
                config.scale_limits(planned).unwrap();
                size_workers(&config.limits, &mut plan, rdma)
                    .unwrap_or_else(|error| panic!("cores={cores} rdma={rdma}: {error:?}"));
                assert_eq!(plan.pairs.len(), planned, "cores={cores} rdma={rdma}");
            }
        }
    }

    #[test]
    fn explicit_small_node_budget_still_rejects_large_plan() {
        // Explicit values remain authoritative node-wide budgets, so the old
        // 256 MiB plaintext value must still fail below the half floor.
        let mut config = operator_profile_config();
        config.enable_rdma = false;
        config.limits.plaintext_bytes = NonZeroUsize::new(256 * 1024 * 1024).unwrap();
        config.scaled_limits.plaintext_bytes = false;
        let mut plan = core_plan(&config, 64);
        assert!(plan.pairs.len() > 10);
        config.scale_limits(plan.pairs.len()).unwrap();
        assert_eq!(
            size_workers(&config.limits, &mut plan, false).unwrap_err(),
            Error::InvalidConfiguration
        );
    }

    fn four_pair_plan(config: &Config) -> AffinityPlan {
        use crate::affinity::CpuLocation;
        use crate::affinity::EffectiveTopology;
        let plan = AffinityPlan::from_topology(
            config,
            EffectiveTopology {
                cpus: (0..8)
                    .map(|cpu| CpuLocation {
                        cpu,
                        package: 0,
                        core: cpu,
                        numa_node: None,
                    })
                    .collect(),
                quota: None,
                nics: vec![],
            },
            &[],
        )
        .unwrap();
        assert_eq!(plan.pairs.len(), 5);
        plan
    }

    #[test]
    fn smt_startup_final_count_partitions_live_budgets_and_obeys_memory_floors() {
        use crate::affinity::CpuLocation;
        use crate::affinity::EffectiveTopology;
        for (allow_smt, expected) in [(false, 3), (true, 5)] {
            let mut config = Config::from_lookup(|name| {
                Ok(match name {
                    "RACER_CLUSTER_ID" => Some("00000000-0000-4000-8000-000000000001".into()),
                    "RACER_CONTROL_ENDPOINT" => Some("https://control.example".into()),
                    "RACER_ALLOW_SMT" => Some(allow_smt.to_string()),
                    "RACER_PLAINTEXT_BYTES" | "RACER_CIPHERTEXT_BYTES" => Some("4294967296".into()),
                    "RACER_RELAY_TRANSFERS" => Some("256".into()),
                    "RACER_PIPES" => Some("128".into()),
                    "RACER_CLIENT_CONNECTIONS" => Some("2048".into()),
                    "RACER_NEIGHBOR_CONNECTIONS_PER_WORKER" => Some("16".into()),
                    "RACER_QUEUE_ENTRIES" => Some("1024".into()),
                    _ => None,
                })
            })
            .unwrap();
            let make_plan = |config: &Config| {
                AffinityPlan::from_topology(
                    config,
                    EffectiveTopology {
                        cpus: (0..8)
                            .map(|cpu| CpuLocation {
                                cpu,
                                package: 0,
                                core: cpu % 4,
                                numa_node: Some(0),
                            })
                            .collect(),
                        quota: None,
                        nics: vec![],
                    },
                    &[],
                )
                .unwrap()
            };
            let mut plan = make_plan(&config);
            config.scale_limits(plan.pairs.len()).unwrap();
            let limits = size_workers(&config.limits, &mut plan, config.enable_rdma).unwrap();
            assert_eq!(plan.pairs.len(), expected);
            for (partition, node) in [
                (limits.plaintext_bytes, 4usize * 1024 * 1024 * 1024),
                (limits.ciphertext_bytes, 4usize * 1024 * 1024 * 1024),
                (limits.relay_transfers, 256),
                (limits.pipes, 128),
                (limits.client_connections, 2048),
                (limits.queue_entries, 1024),
            ] {
                assert_eq!(partition.get(), node / expected);
            }
            assert_eq!(limits.connections_per_neighbor.get(), 16);
            let floor =
                (config.limits.range_window_pages.get() + 1) * crate::model::PAGE_BYTES as usize;
            config.limits.plaintext_bytes = NonZeroUsize::new(2 * floor).unwrap();
            let mut plan = make_plan(&config);
            let result = size_workers(&config.limits, &mut plan, false);
            let limits = if allow_smt {
                assert!(matches!(result, Err(Error::InvalidConfiguration)));
                assert_eq!(plan.pairs.len(), expected);
                super::super::partition_limits(&config.limits, 2, false).unwrap()
            } else {
                assert_eq!(plan.pairs.len(), 2);
                assert_eq!(plan.crypto_groups(), vec![vec![0, 1]]);
                result.unwrap()
            };
            assert_eq!(limits.plaintext_bytes.get(), floor);
            assert_eq!(limits.relay_transfers.get(), 128);
            assert_eq!(plan.crypto_groups().len(), if allow_smt { 3 } else { 1 });
            assert!(
                plan.pairs
                    .iter()
                    .all(|pair| pair.io.numa_node == pair.crypto.numa_node)
            );
            assert_eq!(
                plan.pairs.iter().map(|p| p.worker).collect::<Vec<_>>(),
                (0..if allow_smt { expected } else { 2 })
                    .map(|id| WorkerId(id as u16))
                    .collect::<Vec<_>>()
            );
            config.limits.plaintext_bytes = NonZeroUsize::new(floor - 1).unwrap();
            assert!(size_workers(&config.limits, &mut make_plan(&config), false).is_err());
        }
    }

    #[test]
    fn resource_shortage_reduces_and_regroups_uneven_numa_workers() {
        use crate::affinity::CpuLocation;
        use crate::affinity::EffectiveTopology;
        let mut config = default_config(false);
        config.scale_limits(11).unwrap();
        let floor =
            (config.limits.range_window_pages.get() + 1) * crate::model::PAGE_BYTES as usize;
        for supported in 1..=10 {
            config.limits.plaintext_bytes = NonZeroUsize::new(supported * floor).unwrap();
            config.limits.ciphertext_bytes = NonZeroUsize::new(1024 * 1024 * 1024).unwrap();
            config.limits.dirty_bytes = NonZeroUsize::new(1024 * 1024 * 1024).unwrap();
            config.limits.request_context_bytes = NonZeroUsize::new(1024 * 1024 * 1024).unwrap();
            let mut plan = AffinityPlan::from_topology(
                &config,
                EffectiveTopology {
                    cpus: (0..16)
                        .map(|cpu| CpuLocation {
                            cpu,
                            core: cpu,
                            package: 0,
                            numa_node: Some(usize::from(cpu >= 7)),
                        })
                        .collect(),
                    quota: None,
                    nics: vec![],
                },
                &[],
            )
            .unwrap();
            assert_eq!(plan.pairs.len(), 11);
            let result = size_workers(&config.limits, &mut plan, false);
            let limits = if supported < 6 {
                assert!(matches!(result, Err(Error::InvalidConfiguration)));
                assert_eq!(plan.pairs.len(), 11);
                assert_eq!(
                    super::super::partition_limits_with_cause(&config.limits, 6, false).err(),
                    Some(("plaintext_bytes", Error::InvalidConfiguration))
                );
                // Partition and placement still support small plans; startup may not shed this far.
                plan.reduce_workers(supported);
                super::super::partition_limits(&config.limits, supported, false).unwrap()
            } else {
                result.unwrap()
            };
            assert_eq!(plan.pairs.len(), supported);
            assert_eq!(limits.plaintext_bytes.get(), floor);
            assert!(
                plan.pairs
                    .iter()
                    .all(|pair| pair.io.numa_node == pair.crypto.numa_node)
            );
            for node in [Some(0), Some(1)] {
                let io = plan
                    .pairs
                    .iter()
                    .filter(|pair| pair.io.numa_node == node)
                    .count();
                let crypto = plan
                    .crypto_groups()
                    .iter()
                    .filter(|group| plan.pairs[group[0]].crypto.numa_node == node)
                    .count();
                // The original seven-core node has two crypto CPUs; shrinking
                // never invents more even when rounding five I/O shards upward.
                assert_eq!(
                    crypto,
                    io.div_ceil(2).min(if node == Some(0) { 2 } else { 3 })
                );
            }
        }
    }

    #[test]
    fn native_node_has_one_owner_and_one_client_per_worker() {
        let mut config = default_config(true);
        let mut plan = four_pair_plan(&config);
        config.scale_limits(plan.pairs.len()).unwrap();
        let limits = size_workers(&config.limits, &mut plan, true).unwrap();
        assert!(plan.crypto_groups().len() < plan.pairs.len());
        let native = NativeNode::default();
        native.place(&plan).unwrap();
        native
            .prepare(plan.pairs.iter().map(|pair| pair.worker), &limits)
            .unwrap();
        assert_eq!(native.owner_worker(), Some(plan.pairs[0].worker));
        for pair in &plan.pairs {
            assert_eq!(
                native
                    .numa(pair.worker)
                    .unwrap()
                    .map(|placement| placement.numa_node),
                Some(pair.crypto.numa_node)
            );
            assert!(native.client(pair.worker).unwrap().is_some());
            assert!(native.client(pair.worker).unwrap().is_none());
        }
    }

    #[test]
    /// The owner runs on its own pinned thread: it sleeps without periodic
    /// wakes while idle, a command wakes it and its op completes on the
    /// worker side without a timer, and shutdown drains and joins it.
    fn owner_thread_parks_idle_serves_commands_and_joins() {
        let mut limits = default_config(true).limits;
        limits.registered_bytes = NonZeroUsize::new(crate::rdma::BUFFER_LEN).unwrap();
        let native = NativeNode::default();
        native
            .prepare(std::iter::once(WorkerId(0)), &limits)
            .unwrap();
        let client = native.client(WorkerId(0)).unwrap().unwrap();
        let cpu = *racer_io::pin::current_cpus().unwrap().first().unwrap();
        let owner = native
            .start_owner(cpu, racer_io::WorkerCtx::host())
            .unwrap()
            .unwrap();
        assert!(
            native
                .start_owner(cpu, racer_io::WorkerCtx::host())
                .unwrap()
                .is_none(),
            "one owner per node"
        );
        let idle = owner.wakes();
        std::thread::sleep(Duration::from_millis(200));
        assert!(owner.wakes() - idle <= 1, "idle owner woke periodically");
        // Not activated: the op fails, but the owner must still answer it.
        let key = rdma_verbs::LinkKey {
            peer: [1; 32],
            rail: 0,
        };
        let opened = client.open(key);
        let started = std::time::Instant::now();
        let result = futures::executor::block_on(std::future::poll_fn(|cx| opened.poll(cx)));
        assert!(result.is_err());
        assert!(started.elapsed() < Duration::from_secs(5));
        assert!(owner.wakes() > idle);
        let started = std::time::Instant::now();
        owner.shutdown().unwrap();
        assert!(started.elapsed() < Duration::from_secs(5));
        assert!(client.closed() && client.drained());
    }

    #[test]
    fn activation_turn_scales_with_registered_bytes() {
        assert_eq!(activation_turn(0, 0), Duration::from_secs(5));
        assert_eq!(activation_turn(1, 0), Duration::from_secs(6));
        assert_eq!(activation_turn(1 << 30, 0), Duration::from_secs(6));
        assert_eq!(activation_turn(32 << 30, 0), Duration::from_secs(37));
    }

    #[test]
    /// 8 rails with 8 GiB took about 12 s of serial implicit MR creation
    /// alone, past the old 13 s turn once pool registration was added. The
    /// turn now leaves room for it even if the devices run serially.
    fn activation_turn_covers_per_device_setup() {
        assert_eq!(activation_turn(0, 1), Duration::from_secs(7));
        assert_eq!(activation_turn(8 << 30, 8), Duration::from_secs(29));
        let observed = Duration::from_millis(1500) * 8 + Duration::from_secs(8);
        assert!(activation_turn(8 << 30, 8) > observed);
        // Saturates rather than overflowing.
        assert!(activation_turn(usize::MAX, usize::MAX) > Duration::from_secs(1 << 40));
    }

    #[test]
    /// Failed activations back off exponentially to a cap instead of
    /// retrying every second.
    fn activation_retry_backs_off() {
        assert_eq!(activation_retry_delay(0), Duration::from_secs(1));
        assert_eq!(activation_retry_delay(1), Duration::from_secs(1));
        assert_eq!(activation_retry_delay(2), Duration::from_secs(2));
        assert_eq!(activation_retry_delay(3), Duration::from_secs(4));
        assert_eq!(activation_retry_delay(7), Duration::from_secs(60));
        assert_eq!(activation_retry_delay(u32::MAX), Duration::from_secs(60));
        let total: Duration = (1..=20).map(activation_retry_delay).sum();
        assert!(total > Duration::from_secs(20 * 30), "hot retry loop");
    }

    #[test]
    fn default_worker_sizing_funds_node_wide_native_buffers() {
        let mut config = default_config(true);
        assert_eq!(config.limits.registered_bytes.get(), 384 * 1024 * 1024);
        let mut plan = four_pair_plan(&config);
        config.scale_limits(plan.pairs.len()).unwrap();
        let limits = size_workers(&config.limits, &mut plan, true).unwrap();
        assert_eq!(plan.pairs.len(), 5);
        // One thread of the budget is kept for the RDMA owner.
        assert_eq!(plan.max_threads, usize::MAX - 1);
        for (worker, pair) in plan.pairs.iter().enumerate() {
            assert_eq!(pair.worker, WorkerId(worker as u16));
        }
        for (partition, node) in [
            (limits.plaintext_bytes, config.limits.plaintext_bytes),
            (limits.ciphertext_bytes, config.limits.ciphertext_bytes),
            (limits.dirty_bytes, config.limits.dirty_bytes),
            (
                limits.request_context_bytes,
                config.limits.request_context_bytes,
            ),
            (limits.flights, config.limits.flights),
            (limits.queue_entries, config.limits.queue_entries),
            (limits.client_connections, config.limits.client_connections),
            (limits.pipes, config.limits.pipes),
            (
                limits.placement_cache_bytes,
                config.limits.placement_cache_bytes,
            ),
            (limits.path_cache_bytes, config.limits.path_cache_bytes),
            (limits.cached_paths, config.limits.cached_paths),
            (limits.metadata_entries, config.limits.metadata_entries),
            (limits.relay_transfers, config.limits.relay_transfers),
        ] {
            assert_eq!(partition.get(), node.get() / plan.pairs.len());
            assert!(partition.get() * plan.pairs.len() <= node.get());
        }
        assert_eq!(limits.range_window_pages, config.limits.range_window_pages);
        assert_eq!(
            limits.connections_per_neighbor,
            config.limits.connections_per_neighbor
        );

        // Registered memory is one node-wide pool, never divided per worker.
        assert_eq!(limits.registered_bytes, config.limits.registered_bytes);
        assert_eq!(
            buffer_count(&limits),
            5 * 384 * 1024 * 1024 / crate::rdma::BUFFER_LEN
        );
        let native = NativeNode::default();
        native
            .prepare(plan.pairs.iter().map(|p| p.worker), &limits)
            .unwrap();
        for pair in &plan.pairs {
            assert!(native.client(pair.worker).unwrap().is_some());
        }
    }

    #[test]
    fn native_worker_sizing_requires_one_node_wide_buffer() {
        let mut config = default_config(true);
        config
            .scale_limits(four_pair_plan(&config).pairs.len())
            .unwrap();
        let buffer = crate::rdma::BUFFER_LEN;
        for (budget, expected) in [
            (1, 0),
            (16 * 1024 * 1024 + 16, 0),
            (buffer, 1),
            (2 * buffer - 1, 1),
            (2 * buffer, 2),
            (3 * buffer + 3, 3),
        ] {
            config.limits.registered_bytes = NonZeroUsize::new(budget).unwrap();
            let mut plan = four_pair_plan(&config);
            let result = size_workers(&config.limits, &mut plan, true);
            assert_eq!(plan.pairs.len(), 5, "budget={budget}");
            if expected == 0 {
                assert!(
                    matches!(result, Err(Error::InvalidConfiguration)),
                    "budget={budget}"
                );
                assert_eq!(
                    super::super::partition_limits_with_cause(&config.limits, 1, true).err(),
                    Some(("registered_bytes", Error::InvalidConfiguration))
                );
            } else {
                let limits = result.unwrap();
                assert_eq!(buffer_count(&limits), expected, "budget={budget}");
                assert_eq!(limits.registered_bytes.get(), budget);
            }
        }
        let mut plan = four_pair_plan(&config);
        plan.pairs.clear();
        assert!(matches!(
            size_workers(&config.limits, &mut plan, true),
            Err(Error::InvalidConfiguration)
        ));
    }

    #[test]
    fn http_worker_sizing_ignores_unused_native_capacity() {
        let mut config = default_config(false);
        config
            .scale_limits(four_pair_plan(&config).pairs.len())
            .unwrap();
        for budget in [1, 128 * 1024 * 1024] {
            config.limits.registered_bytes = NonZeroUsize::new(budget).unwrap();
            let mut plan = four_pair_plan(&config);
            let limits = size_workers(&config.limits, &mut plan, false).unwrap();
            assert_eq!(plan.pairs.len(), 5);
            assert_eq!(limits.registered_bytes, config.limits.registered_bytes);
            assert_eq!(
                limits.plaintext_bytes.get(),
                config.limits.plaintext_bytes.get() / 5
            );
            // Native sizing must still honor all the existing non-native floors.
            config.limits.pipes = NonZeroUsize::new(2).unwrap();
            for rdma in [false, true] {
                if rdma && budget == 1 {
                    continue;
                }
                let mut plan = four_pair_plan(&config);
                assert!(matches!(
                    size_workers(&config.limits, &mut plan, rdma),
                    Err(Error::InvalidConfiguration)
                ));
                assert_eq!(plan.pairs.len(), 5);
                let limits = super::super::partition_limits(&config.limits, 2, rdma).unwrap();
                assert_eq!(limits.pipes.get(), 1);
            }
            config.limits.pipes = NonZeroUsize::new(16).unwrap();
        }
    }

    #[test]
    fn native_preparation_funds_one_buffer_or_falls_back_to_http() {
        let mut limits = default_config(true).limits;
        let buffer = crate::rdma::BUFFER_LEN;
        for (budget, funded) in [(buffer - 1, false), (buffer, true), (2 * buffer, true)] {
            limits.registered_bytes = NonZeroUsize::new(budget).unwrap();
            let native = NativeNode::default();
            native
                .prepare([WorkerId(0), WorkerId(1)].into_iter(), &limits)
                .unwrap();
            assert_eq!(native.client(WorkerId(1)).unwrap().is_some(), funded);
            assert_eq!(native.owner_worker(), funded.then_some(WorkerId(0)));
        }
    }

    #[test]
    fn native_activation_exhaustion_fails_closed_and_rolls_back_partial_quota() {
        let mut limits = default_config(true).limits;
        limits.registered_bytes = NonZeroUsize::new(4 * crate::rdma::BUFFER_LEN).unwrap();
        let native = NativeNode::default();
        native
            .prepare(std::iter::once(WorkerId(0)), &limits)
            .unwrap();
        let links = crate::rdma::Links::new(
            native.client(WorkerId(0)).unwrap().unwrap(),
            native.link_node(),
            NodeId("node".into()),
            None,
            racer_io::WorkerCtx::host(),
        );
        let admission = flow_control::Quotas::new(AdmissionPolicy::new(limits.clone()));
        let held = admission
            .reserve(None, ResourceClass::Registered, 1)
            .unwrap();
        let startup = scope(&racer_io::Clock::host(), Duration::from_secs(10)).unwrap();
        let publication = vec![racer_wire::control::RailMapping {
            device: "missing".into(),
            port: 1,
            gid: None,
            rail: RailId(0),
            numa_node: None,
        }];
        assert!(matches!(
            futures::executor::block_on(links.activate(
                publication,
                &admission,
                limits.registered_bytes.get(),
                &startup,
            )),
            Err(Error::Overloaded)
        ));
        assert_eq!(admission.used(ResourceClass::Registered), 1);
        assert!(!links.rail_ready(RailId(0)));
        assert!(links.node().settled());
        drop(held);
        assert_eq!(admission.used(ResourceClass::Registered), 0);
    }

    fn configured() -> Application {
        let mut config = Config::from_lookup(|name| {
            Ok(match name {
                "RACER_CLUSTER_ID" => Some("00000000-0000-4000-8000-000000000001".into()),
                "RACER_CONTROL_ENDPOINT" => Some("https://control.example".into()),
                "RACER_ENABLE_RDMA" => Some("true".into()),
                "RACER_MAX_THREADS" => Some("2".into()),
                _ => None,
            })
        })
        .unwrap();
        // Stand in only for the identity returned by authenticated enrollment.
        config.node = NodeId("00000000-0000-4000-8000-000000000002".into());
        let mut app = Application::assemble(config).unwrap();
        app.discovered_nics = vec![racer_wire::control::RailMapping {
            device: "missing".into(),
            port: 1,
            gid: Some([1; 16]),
            rail: RailId(7),
            numa_node: None,
        }];
        app
    }

    fn worker(
        env: &racer_io::Env,
        app: &Application,
        aligned: bool,
        published_rails: bool,
    ) -> (WorkerApplication, CryptoRuntime) {
        app.node
            .native
            .prepare(std::iter::once(WorkerId(0)), &app.limits)
            .unwrap();
        let admission = Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(
            app.limits.clone(),
        )));
        let (io, engine) = security::pair(WorkerId(0), 0, app.limits.queue_entries);
        let runtime = WorkerRuntime {
            reactor: Rc::new(Reactor::new(admission.clone(), env)),
            admission,
            crypto: Rc::new(CryptoClient::new(io)),
        };
        let mut worker = WorkerApplication::assemble(
            &app.config,
            app.node.clone(),
            WorkerId(0),
            runtime,
            app.discovered_nics.clone(),
        )
        .unwrap();
        worker.discovered_nics = app.discovered_nics.clone();
        worker
            .snapshots
            .apply(
                Publication {
                    pools: Vec::new(),
                    schema_version: racer_wire::control::SCHEMA_VERSION,
                    cluster: app.config.cluster.clone(),
                    sequence: PublicationSequence(1),
                    membership_version: MembershipVersion(1),
                    members: vec![racer_wire::control::Member {
                        node: app.config.node.clone(),
                        shares: std::num::NonZeroU32::new(1).unwrap(),
                        peer_endpoint: "127.0.0.1:7443".into(),
                        rails: if published_rails && aligned {
                            app.discovered_nics.clone()
                        } else {
                            vec![]
                        },
                        site: "site1".into(),
                    }],
                    caches: vec![],
                    page_size: racer_wire::object::PageSize::DEFAULT,
                }
                .with_default_pool(),
            )
            .unwrap();
        (
            worker,
            CryptoRuntime {
                port: engine,
                ctx: racer_io::WorkerCtx::host(),
            },
        )
    }

    #[test]
    fn native_worker_selects_only_funded_local_rails_and_recovers() {
        use rdma_verbs::simulation::Device;
        use rdma_verbs::simulation::Simulation;
        let sim = Simulation::new();
        let simulated = sim
            .with_devices(vec![Device::new("test-device", [1; 16])])
            .unwrap();
        let _devices = simulated.enter();
        let env = racer_io::Env::host();
        let mut app = configured();
        app.discovered_nics[0].device = "test-device".into();
        let (mut worker, engine) = worker(&env, &app, true, true);
        assert_eq!(worker.native_publication().unwrap().len(), 1);
        let mut service = test_owner(&app, engine);
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        for _ in 0..20 {
            worker.poll_native(&mut cx).unwrap();
            service
                .poll_budgeted(&mut Context::from_waker(futures::task::noop_waker_ref()), 8)
                .unwrap();
        }
        assert!(worker.links.as_ref().unwrap().rail_ready(RailId(7)));
        let charged = worker.runtime.admission.used(ResourceClass::Registered);
        assert!(charged > 0);
        worker.links.as_ref().unwrap().close();
        worker.actual_rails.clear();
        service
            .poll_budgeted(
                &mut Context::from_waker(futures::task::noop_waker_ref()),
                256,
            )
            .unwrap();
        assert_eq!(worker.runtime.admission.used(ResourceClass::Registered), 0);
        worker.native_retry = racer_io::Clock::host().now();
        for _ in 0..20 {
            worker.poll_native(&mut cx).unwrap();
            service
                .poll_budgeted(&mut Context::from_waker(futures::task::noop_waker_ref()), 8)
                .unwrap();
        }
        assert!(worker.links.as_ref().unwrap().rail_ready(RailId(7)));
        assert_eq!(
            worker.runtime.admission.used(ResourceClass::Registered),
            charged
        );
        worker
            .snapshots
            .apply(
                Publication {
                    pools: Vec::new(),
                    schema_version: racer_wire::control::SCHEMA_VERSION,
                    cluster: app.config.cluster.clone(),
                    sequence: PublicationSequence(2),
                    membership_version: MembershipVersion(2),
                    members: vec![racer_wire::control::Member {
                        node: app.config.node.clone(),
                        shares: std::num::NonZeroU32::new(1).unwrap(),
                        peer_endpoint: "127.0.0.1:7443".into(),
                        rails: vec![],
                        site: "site1".into(),
                    }],
                    caches: vec![],
                    page_size: racer_wire::object::PageSize::DEFAULT,
                }
                .with_default_pool(),
            )
            .unwrap();
        worker
            .refresh_snapshot(&scope(&racer_io::Clock::host(), Duration::from_secs(1)).unwrap())
            .unwrap();
        assert!(worker.actual_rails.is_empty());
        assert!(!worker.links.as_ref().unwrap().rail_ready(RailId(7)));
        service
            .poll_budgeted(
                &mut Context::from_waker(futures::task::noop_waker_ref()),
                256,
            )
            .unwrap();
        assert_eq!(worker.runtime.admission.used(ResourceClass::Registered), 0);
        drop(service);
        assert_eq!(sim.live_resources(), 0);
    }
    #[test]
    /// Regression: a healthy rail is never reported lost when the node's
    /// published rail number differs from the inventory's reservation for
    /// its device (the inventory also holds ports the mapping excludes,
    /// sorted first). The probe used to read another device's key by rail
    /// number and looped between lost and reactivated forever.
    fn healthy_device_with_shifted_inventory_rail_is_not_lost() {
        use crate::telemetry::Event;
        use rdma_verbs::simulation::Device;
        use rdma_verbs::simulation::Simulation;
        use std::os::unix::fs::symlink;
        struct Scratch(std::path::PathBuf);
        impl Drop for Scratch {
            fn drop(&mut self) {
                let _ = std::fs::remove_dir_all(&self.0);
            }
        }
        let dir = Scratch(
            std::path::Path::new(env!("CARGO_MANIFEST_DIR"))
                .join("target")
                .join(format!("shifted-rail-{}", std::process::id())),
        );
        let root = dir.0.join("infiniband");
        let pci = dir.0.join("7870:00:00.0");
        let other_pci = dir.0.join("0000:d2:00.0");
        std::fs::create_dir_all(&pci).unwrap();
        std::fs::create_dir_all(&other_pci).unwrap();
        for (name, target) in [("test-device", &pci), ("ibp210s0f0", &other_pci)] {
            std::fs::create_dir_all(root.join(name)).unwrap();
            symlink(target, root.join(name).join("device")).unwrap();
        }
        let sim = Simulation::new();
        let simulated = sim
            .with_devices(vec![Device::new("test-device", [1; 16])])
            .unwrap();
        let _devices = simulated.enter();
        let env = racer_io::Env::host();
        let mut app = configured();
        app.discovered_nics[0].device = "test-device".into();
        app.discovered_nics[0].rail = RailId(0);
        let mut excluded = app.discovered_nics[0].clone();
        excluded.device = "ibp210s0f0".into();
        // The excluded port takes inventory rail 0; the published device
        // gets inventory rail 1 but is published as rail 0.
        app.node
            .inventory
            .update_keyed(vec![
                (excluded, Some("0000:d2:00.0".into())),
                (app.discovered_nics[0].clone(), Some("7870:00:00.0".into())),
            ])
            .unwrap();
        let (mut worker, engine) = worker(&env, &app, true, true);
        worker.rdma_sysfs = root.clone();
        worker.rail_settle = Duration::ZERO;
        let mut service = test_owner(&app, engine);
        let mut run = |worker: &mut WorkerApplication| {
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            for _ in 0..20 {
                worker.poll_native(&mut cx).unwrap();
                service.poll_budgeted(&mut cx, 64).unwrap();
            }
        };
        run(&mut worker);
        let links = worker.links.clone().unwrap();
        assert!(links.rail_ready(RailId(0)));
        let metrics = worker.telemetry.metrics.clone();
        for _ in 0..3 {
            worker.rail_probe = worker.ctx().now();
            run(&mut worker);
        }
        assert!(links.rail_ready(RailId(0)));
        assert!(worker.rdma_lost.is_none());
        assert_eq!(metrics.count(Event::RdmaDeviceLoss), 0);
    }

    #[test]
    /// A rail whose device leaves sysfs (an Azure VF hot-unplug) closes the
    /// round at once so reads use HTTP, activation waits while the device is
    /// missing, and RDMA reactivates by itself once it is back.
    fn lost_device_falls_back_and_reactivates_when_back() {
        use crate::telemetry::{Event, Gauge};
        use rdma_verbs::simulation::Device;
        use rdma_verbs::simulation::Simulation;
        use std::os::unix::fs::symlink;
        struct Scratch(std::path::PathBuf);
        impl Drop for Scratch {
            fn drop(&mut self) {
                let _ = std::fs::remove_dir_all(&self.0);
            }
        }
        let dir = Scratch(
            std::path::Path::new(env!("CARGO_MANIFEST_DIR"))
                .join("target")
                .join(format!("lost-device-{}", std::process::id())),
        );
        let root = dir.0.join("infiniband");
        let pci = dir.0.join("7870:00:00.0");
        std::fs::create_dir_all(&root).unwrap();
        std::fs::create_dir_all(&pci).unwrap();
        let plug = || {
            std::fs::create_dir_all(root.join("test-device")).unwrap();
            symlink(&pci, root.join("test-device").join("device")).unwrap();
        };
        plug();
        let sim = Simulation::new();
        let simulated = sim
            .with_devices(vec![Device::new("test-device", [1; 16])])
            .unwrap();
        let _devices = simulated.enter();
        let env = racer_io::Env::host();
        let mut app = configured();
        app.discovered_nics[0].device = "test-device".into();
        app.discovered_nics[0].rail = RailId(0);
        app.node
            .inventory
            .update_keyed(vec![(
                app.discovered_nics[0].clone(),
                Some("7870:00:00.0".into()),
            )])
            .unwrap();
        let (mut worker, engine) = worker(&env, &app, true, true);
        worker.rdma_sysfs = root.clone();
        worker.rail_settle = Duration::ZERO;
        let mut service = test_owner(&app, engine);
        let mut run = |worker: &mut WorkerApplication| {
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            for _ in 0..20 {
                worker.poll_native(&mut cx).unwrap();
                service.poll_budgeted(&mut cx, 64).unwrap();
            }
        };
        run(&mut worker);
        let links = worker.links.clone().unwrap();
        assert!(links.rail_ready(RailId(0)));
        let metrics = worker.telemetry.metrics.clone();
        // Unplug: the next probe closes the round.
        std::fs::remove_dir_all(root.join("test-device")).unwrap();
        worker.rail_probe = worker.ctx().now();
        run(&mut worker);
        assert!(!links.rail_ready(RailId(0)), "reads fall back to HTTP");
        assert!(worker.actual_rails.is_empty());
        assert!(worker.rdma_lost.is_some());
        assert_eq!(metrics.count(Event::RdmaDeviceLoss), 1);
        assert_eq!(metrics.count(Event::RdmaActivationFailure), 0);
        // The owner learns of the loss sysfs showed, so the round drains
        // without waiting for pages no NIC can reach.
        assert_eq!(links.device_losses(), 1);
        // Still missing: no activation is attempted.
        worker.native_retry = worker.ctx().now();
        run(&mut worker);
        assert!(worker.native_task.is_none());
        assert!(!links.rail_ready(RailId(0)));
        assert_eq!(
            worker.rdma_waiting.as_deref(),
            Some("device_missing:test-device")
        );
        assert_eq!(metrics.gauge(Gauge::RdmaWaitingDevice), 1);
        assert_eq!(metrics.gauge(Gauge::RdmaWaitingDrain), 0);
        assert_eq!(metrics.count(Event::RdmaActivationFailure), 0);
        // Replug: activation resumes and the rail is ready again.
        plug();
        worker.native_retry = worker.ctx().now();
        run(&mut worker);
        assert!(links.rail_ready(RailId(0)));
        assert!(worker.rdma_lost.is_none());
        assert_eq!(worker.rdma_waiting, None);
        assert_eq!(metrics.gauge(Gauge::RdmaWaitingDevice), 0);
        assert_eq!(metrics.count(Event::RdmaDeviceReactivation), 1);
        assert_eq!(metrics.count(Event::RdmaDeviceLoss), 1);
        // Healthy probes change nothing.
        worker.rail_probe = worker.ctx().now();
        run(&mut worker);
        assert!(links.rail_ready(RailId(0)));
        assert_eq!(metrics.count(Event::RdmaDeviceLoss), 1);
    }

    /// Apply a publication listing this node with `rails`.
    fn publish_rails(
        worker: &mut WorkerApplication,
        app: &Application,
        sequence: u64,
        rails: Vec<racer_wire::control::RailMapping>,
    ) {
        worker
            .snapshots
            .apply(
                Publication {
                    pools: Vec::new(),
                    schema_version: racer_wire::control::SCHEMA_VERSION,
                    cluster: app.config.cluster.clone(),
                    sequence: PublicationSequence(sequence),
                    membership_version: MembershipVersion(sequence),
                    members: vec![racer_wire::control::Member {
                        node: app.config.node.clone(),
                        shares: std::num::NonZeroU32::new(1).unwrap(),
                        peer_endpoint: "127.0.0.1:7443".into(),
                        rails,
                        site: "site1".into(),
                    }],
                    caches: vec![],
                    page_size: racer_wire::object::PageSize::DEFAULT,
                }
                .with_default_pool(),
            )
            .unwrap();
    }

    #[test]
    /// A device back before the old round drained waits for the drain,
    /// says so in the log and the `racer_rdma_reactivation_waiting_drain`
    /// gauge, and reactivates once the owner drains.
    fn lost_device_waits_for_drain_then_reactivates() {
        use crate::telemetry::{Event, Gauge};
        use rdma_verbs::simulation::Device;
        use rdma_verbs::simulation::Simulation;
        use std::os::unix::fs::symlink;
        struct Scratch(std::path::PathBuf);
        impl Drop for Scratch {
            fn drop(&mut self) {
                let _ = std::fs::remove_dir_all(&self.0);
            }
        }
        let dir = Scratch(
            std::path::Path::new(env!("CARGO_MANIFEST_DIR"))
                .join("target")
                .join(format!("undrained-device-{}", std::process::id())),
        );
        let root = dir.0.join("infiniband");
        let pci = dir.0.join("7870:00:00.0");
        std::fs::create_dir_all(&root).unwrap();
        std::fs::create_dir_all(&pci).unwrap();
        let plug = || {
            std::fs::create_dir_all(root.join("mana_0")).unwrap();
            symlink(&pci, root.join("mana_0").join("device")).unwrap();
        };
        plug();
        let sim = Simulation::new();
        let simulated = sim
            .with_devices(vec![Device::new("mana_0", [1; 16])])
            .unwrap();
        let _devices = simulated.enter();
        let env = racer_io::Env::host();
        let mut app = configured();
        app.discovered_nics[0].device = "mana_0".into();
        app.discovered_nics[0].rail = RailId(0);
        app.node
            .inventory
            .update_keyed(vec![(
                app.discovered_nics[0].clone(),
                Some("7870:00:00.0".into()),
            )])
            .unwrap();
        let (mut worker, engine) = worker(&env, &app, true, true);
        worker.rdma_sysfs = root.clone();
        worker.rail_settle = Duration::ZERO;
        let mut service = test_owner(&app, engine);
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        for _ in 0..20 {
            worker.poll_native(&mut cx).unwrap();
            service.poll_budgeted(&mut cx, 64).unwrap();
        }
        let links = worker.links.clone().unwrap();
        assert!(links.rail_ready(RailId(0)));
        let metrics = worker.telemetry.metrics.clone();
        // Unplug and replug before the owner runs: the round is closed but
        // not drained.
        std::fs::remove_dir_all(root.join("mana_0")).unwrap();
        worker.rail_probe = worker.ctx().now();
        worker.poll_native(&mut cx).unwrap();
        assert!(worker.rdma_lost.is_some());
        plug();
        worker.native_retry = worker.ctx().now();
        worker.poll_native(&mut cx).unwrap();
        assert!(
            worker.native_task.is_none(),
            "activation waits for the drain"
        );
        assert!(
            worker
                .rdma_waiting
                .as_deref()
                .is_some_and(|w| w.starts_with("drain_")),
            "{:?}",
            worker.rdma_waiting
        );
        assert_eq!(metrics.gauge(Gauge::RdmaWaitingDrain), 1);
        assert_eq!(metrics.gauge(Gauge::RdmaWaitingDevice), 0);
        for _ in 0..20 {
            service.poll_budgeted(&mut cx, 64).unwrap();
            worker.native_retry = worker.ctx().now();
            worker.poll_native(&mut cx).unwrap();
        }
        assert!(links.rail_ready(RailId(0)), "RDMA reactivates");
        assert!(worker.rdma_lost.is_none());
        assert_eq!(worker.rdma_waiting, None);
        assert_eq!(metrics.gauge(Gauge::RdmaWaitingDrain), 0);
        assert_eq!(metrics.count(Event::RdmaDeviceReactivation), 1);
    }

    #[test]
    /// Regression (live on Azure MANA): a re-enrollment refreshed the shared
    /// inventory while the device was unbound, so it listed no rail. The
    /// device came back under the same name, and reactivation found an
    /// empty publication forever. It now rediscovers the device.
    fn lost_device_reactivates_after_inventory_dropped_it() {
        use crate::telemetry::Event;
        use rdma_verbs::simulation::Device;
        use rdma_verbs::simulation::Simulation;
        use std::os::unix::fs::symlink;
        struct Scratch(std::path::PathBuf);
        impl Drop for Scratch {
            fn drop(&mut self) {
                let _ = std::fs::remove_dir_all(&self.0);
            }
        }
        let dir = Scratch(
            std::path::Path::new(env!("CARGO_MANIFEST_DIR"))
                .join("target")
                .join(format!("dropped-device-{}", std::process::id())),
        );
        let root = dir.0.join("infiniband");
        let pci = dir.0.join("7870:00:00.0");
        std::fs::create_dir_all(&root).unwrap();
        std::fs::create_dir_all(&pci).unwrap();
        let plug = || {
            std::fs::create_dir_all(root.join("mana_0")).unwrap();
            symlink(&pci, root.join("mana_0").join("device")).unwrap();
        };
        plug();
        let sim = Simulation::new();
        let simulated = sim
            .with_devices(vec![Device::new("mana_0", [1; 16])])
            .unwrap();
        let _devices = simulated.enter();
        let env = racer_io::Env::host();
        let mut app = configured();
        app.discovered_nics[0].device = "mana_0".into();
        app.discovered_nics[0].rail = RailId(0);
        app.node
            .inventory
            .update_keyed(vec![(
                app.discovered_nics[0].clone(),
                Some("7870:00:00.0".into()),
            )])
            .unwrap();
        let (mut worker, engine) = worker(&env, &app, true, true);
        worker.rdma_sysfs = root.clone();
        worker.rail_settle = Duration::ZERO;
        let mut service = test_owner(&app, engine);
        let mut run = |worker: &mut WorkerApplication| {
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            for _ in 0..20 {
                worker.poll_native(&mut cx).unwrap();
                service.poll_budgeted(&mut cx, 64).unwrap();
            }
        };
        run(&mut worker);
        let links = worker.links.clone().unwrap();
        assert!(links.rail_ready(RailId(0)));
        let metrics = worker.telemetry.metrics.clone();
        std::fs::remove_dir_all(root.join("mana_0")).unwrap();
        worker.rail_probe = worker.ctx().now();
        run(&mut worker);
        assert!(worker.rdma_lost.is_some());
        // The inventory refreshes while the device is gone.
        app.node.inventory.update(Vec::new()).unwrap();
        worker.native_retry = worker.ctx().now();
        run(&mut worker);
        assert!(worker.discovered_nics.is_empty());
        plug();
        worker.native_retry = worker.ctx().now();
        run(&mut worker);
        assert!(links.rail_ready(RailId(0)), "RDMA reactivates");
        assert!(worker.rdma_lost.is_none());
        assert_eq!(worker.rdma_waiting, None);
        assert_eq!(metrics.count(Event::RdmaDeviceReactivation), 1);
    }

    #[test]
    /// A replugged device is not listed until its names held still for
    /// `RAIL_SETTLE`: libibverbs would keep a name udev is about to change.
    fn lost_device_reactivates_only_once_its_names_settle() {
        use crate::telemetry::Event;
        use rdma_verbs::simulation::Device;
        use rdma_verbs::simulation::Simulation;
        use std::os::unix::fs::symlink;
        struct Scratch(std::path::PathBuf);
        impl Drop for Scratch {
            fn drop(&mut self) {
                let _ = std::fs::remove_dir_all(&self.0);
            }
        }
        let dir = Scratch(
            std::path::Path::new(env!("CARGO_MANIFEST_DIR"))
                .join("target")
                .join(format!("settling-device-{}", std::process::id())),
        );
        let root = dir.0.join("infiniband");
        let pci = dir.0.join("7870:00:00.0");
        std::fs::create_dir_all(&root).unwrap();
        std::fs::create_dir_all(&pci).unwrap();
        let plug = || {
            std::fs::create_dir_all(root.join("mana_0")).unwrap();
            symlink(&pci, root.join("mana_0").join("device")).unwrap();
        };
        plug();
        let sim = Simulation::new();
        let simulated = sim
            .with_devices(vec![Device::new("mana_0", [1; 16])])
            .unwrap();
        let _devices = simulated.enter();
        let env = racer_io::Env::host();
        let mut app = configured();
        app.discovered_nics[0].device = "mana_0".into();
        app.discovered_nics[0].rail = RailId(0);
        app.node
            .inventory
            .update_keyed(vec![(
                app.discovered_nics[0].clone(),
                Some("7870:00:00.0".into()),
            )])
            .unwrap();
        let (mut worker, engine) = worker(&env, &app, true, true);
        worker.rdma_sysfs = root.clone();
        worker.rdma_char_devices = Some((dir.0.join("infiniband_verbs"), dir.0.join("char")));
        let mut service = test_owner(&app, engine);
        let mut run = |worker: &mut WorkerApplication| {
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            for _ in 0..20 {
                worker.poll_native(&mut cx).unwrap();
                service.poll_budgeted(&mut cx, 64).unwrap();
            }
        };
        run(&mut worker);
        let links = worker.links.clone().unwrap();
        assert!(links.rail_ready(RailId(0)));
        let metrics = worker.telemetry.metrics.clone();
        std::fs::remove_dir_all(root.join("mana_0")).unwrap();
        worker.rail_probe = worker.ctx().now();
        run(&mut worker);
        assert!(worker.rdma_lost.is_some());
        // The inventory refreshes while the device is gone.
        app.node.inventory.update(Vec::new()).unwrap();
        worker.native_retry = worker.ctx().now();
        run(&mut worker);
        assert!(worker.discovered_nics.is_empty());
        plug();
        worker.native_retry = worker.ctx().now();
        run(&mut worker);
        assert_eq!(worker.rdma_waiting.as_deref(), Some("device_settling"));
        assert!(!links.rail_ready(RailId(0)));
        // udev renames it: settling starts over.
        std::fs::rename(root.join("mana_0"), root.join("roceP0")).unwrap();
        worker.rdma_settle.as_mut().unwrap().0 -= RAIL_SETTLE;
        worker.native_retry = worker.ctx().now();
        run(&mut worker);
        assert_eq!(worker.rdma_waiting.as_deref(), Some("device_settling"));
        std::fs::rename(root.join("roceP0"), root.join("mana_0")).unwrap();
        worker.native_retry = worker.ctx().now();
        run(&mut worker);
        worker.rdma_settle.as_mut().unwrap().0 -= RAIL_SETTLE;
        worker.native_retry = worker.ctx().now();
        run(&mut worker);
        assert!(links.rail_ready(RailId(0)), "RDMA reactivates");
        assert!(
            worker.rdma_char_refreshed,
            "fresh character device nodes before reopening"
        );
        assert_eq!(metrics.count(Event::RdmaDeviceReactivation), 1);
    }

    #[test]
    /// Regression (live on Azure MANA): the device came back, but this
    /// process's verbs device list kept it under the name it had before udev
    /// renamed it, so discovery never listed it again. After
    /// `REACTIVATION_ATTEMPTS` the worker fails, so the process restarts.
    fn lost_device_that_stays_undiscovered_restarts_the_process() {
        use crate::telemetry::Event;
        use rdma_verbs::simulation::Device;
        use rdma_verbs::simulation::Simulation;
        use std::os::unix::fs::symlink;
        struct Scratch(std::path::PathBuf);
        impl Drop for Scratch {
            fn drop(&mut self) {
                let _ = std::fs::remove_dir_all(&self.0);
            }
        }
        let dir = Scratch(
            std::path::Path::new(env!("CARGO_MANIFEST_DIR"))
                .join("target")
                .join(format!("stale-device-{}", std::process::id())),
        );
        let root = dir.0.join("infiniband");
        let pci = dir.0.join("7870:00:00.0");
        std::fs::create_dir_all(&root).unwrap();
        std::fs::create_dir_all(&pci).unwrap();
        let plug = || {
            std::fs::create_dir_all(root.join("mana_0")).unwrap();
            symlink(&pci, root.join("mana_0").join("device")).unwrap();
        };
        plug();
        let sim = Simulation::new();
        let simulated = sim
            .with_devices(vec![Device::new("mana_0", [1; 16])])
            .unwrap();
        let _devices = simulated.enter();
        let env = racer_io::Env::host();
        let mut app = configured();
        app.discovered_nics[0].device = "mana_0".into();
        app.discovered_nics[0].rail = RailId(0);
        app.node
            .inventory
            .update_keyed(vec![(
                app.discovered_nics[0].clone(),
                Some("7870:00:00.0".into()),
            )])
            .unwrap();
        let (mut worker, engine) = worker(&env, &app, true, true);
        worker.rdma_sysfs = root.clone();
        worker.rail_settle = Duration::ZERO;
        let mut service = test_owner(&app, engine);
        let mut run = |worker: &mut WorkerApplication| {
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            for _ in 0..20 {
                worker.poll_native(&mut cx).unwrap();
                service.poll_budgeted(&mut cx, 64).unwrap();
            }
        };
        run(&mut worker);
        let links = worker.links.clone().unwrap();
        assert!(links.rail_ready(RailId(0)));
        let metrics = worker.telemetry.metrics.clone();
        std::fs::remove_dir_all(root.join("mana_0")).unwrap();
        worker.rail_probe = worker.ctx().now();
        run(&mut worker);
        assert!(worker.rdma_lost.is_some());
        // The inventory refreshes while the device is gone.
        app.node.inventory.update(Vec::new()).unwrap();
        worker.native_retry = worker.ctx().now();
        run(&mut worker);
        assert!(worker.discovered_nics.is_empty());
        // Back under a name verbs does not list.
        std::fs::create_dir_all(root.join("roceP0")).unwrap();
        std::os::unix::fs::symlink(&pci, root.join("roceP0").join("device")).unwrap();
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        let mut result = Ok(());
        for _ in 0..REACTIVATION_ATTEMPTS * 2 {
            service.poll_budgeted(&mut cx, 64).unwrap();
            worker.native_retry = worker.ctx().now();
            result = worker.poll_native(&mut cx);
            if result.is_err() {
                break;
            }
        }
        assert_eq!(result, Err(Error::Unavailable));
        assert_eq!(worker.rdma_attempts, REACTIVATION_ATTEMPTS);
        assert_eq!(worker.rdma_waiting.as_deref(), Some("device_undiscovered"));
        assert!(!links.rail_ready(RailId(0)));
        assert_eq!(metrics.count(Event::RdmaDeviceReactivation), 0);
    }

    #[test]
    /// Regression: after a VF unplug and replug, when membership has already
    /// dropped the withdrawn rails, RDMA still reactivates from the
    /// discovered rails, and stays active until membership lists them again.
    /// (Renamed replugs are covered by `vf_replug_with_null_gid_twin_keeps_rail`
    /// and `lost_devices_back`; the simulated owner cannot rename devices.)
    fn lost_device_reactivates_after_membership_withdrawal() {
        use crate::telemetry::Event;
        use rdma_verbs::simulation::Device;
        use rdma_verbs::simulation::Simulation;
        use std::os::unix::fs::symlink;
        struct Scratch(std::path::PathBuf);
        impl Drop for Scratch {
            fn drop(&mut self) {
                let _ = std::fs::remove_dir_all(&self.0);
            }
        }
        let dir = Scratch(
            std::path::Path::new(env!("CARGO_MANIFEST_DIR"))
                .join("target")
                .join(format!("withdrawn-device-{}", std::process::id())),
        );
        let root = dir.0.join("infiniband");
        let pci = dir.0.join("7870:00:00.0");
        std::fs::create_dir_all(&root).unwrap();
        std::fs::create_dir_all(&pci).unwrap();
        let plug = || {
            std::fs::create_dir_all(root.join("mana_0")).unwrap();
            symlink(&pci, root.join("mana_0").join("device")).unwrap();
        };
        plug();
        let sim = Simulation::new();
        let simulated = sim
            .with_devices(vec![Device::new("mana_0", [1; 16])])
            .unwrap();
        let _devices = simulated.enter();
        let env = racer_io::Env::host();
        let mut app = configured();
        app.discovered_nics[0].device = "mana_0".into();
        app.discovered_nics[0].rail = RailId(0);
        app.node
            .inventory
            .update_keyed(vec![(
                app.discovered_nics[0].clone(),
                Some("7870:00:00.0".into()),
            )])
            .unwrap();
        let (mut worker, engine) = worker(&env, &app, true, true);
        worker.rdma_sysfs = root.clone();
        worker.rail_settle = Duration::ZERO;
        let mut service = test_owner(&app, engine);
        let mut run = |worker: &mut WorkerApplication| {
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            for _ in 0..20 {
                worker.poll_native(&mut cx).unwrap();
                service.poll_budgeted(&mut cx, 64).unwrap();
            }
        };
        run(&mut worker);
        let links = worker.links.clone().unwrap();
        assert!(links.rail_ready(RailId(0)));
        let metrics = worker.telemetry.metrics.clone();
        // Unplug, and the controller republishes the withdrawn (empty) rails.
        std::fs::remove_dir_all(root.join("mana_0")).unwrap();
        worker.rail_probe = worker.ctx().now();
        run(&mut worker);
        assert!(worker.rdma_lost.is_some());
        assert!(worker.rdma_withdrawn);
        publish_rails(&mut worker, &app, 2, Vec::new());
        plug();
        worker.native_retry = worker.ctx().now();
        run(&mut worker);
        assert!(links.rail_ready(RailId(0)), "RDMA reactivates");
        assert!(worker.rdma_lost.is_none());
        assert!(!worker.rdma_withdrawn);
        assert!(worker.rdma_readvertised);
        assert_eq!(metrics.count(Event::RdmaDeviceReactivation), 1);
        // Membership still lists no rails: the active rails stay compatible.
        let startup = scope(&racer_io::Clock::host(), Duration::from_secs(1)).unwrap();
        worker.refresh_snapshot(&startup).unwrap();
        run(&mut worker);
        assert!(links.rail_ready(RailId(0)));
        // The controller republishes the advertised rails.
        let mut advertised = worker.discovered_nics.clone();
        advertised[0].numa_node = None;
        publish_rails(&mut worker, &app, 3, advertised);
        worker.refresh_snapshot(&startup).unwrap();
        run(&mut worker);
        assert!(!worker.rdma_readvertised);
        assert!(links.rail_ready(RailId(0)));
        assert_eq!(metrics.count(Event::RdmaDeviceLoss), 1);
    }

    #[test]
    /// Regression: a failed activation withdraws the rails, and the retry
    /// still activates once membership lists none for this node.
    fn failed_activation_retries_after_membership_withdrawal() {
        use rdma_verbs::simulation::Device;
        use rdma_verbs::simulation::Simulation;
        let sim = Simulation::new()
            .with_devices(vec![Device::new("test-device", [1; 16])])
            .unwrap();
        let _devices = sim.enter();
        let env = racer_io::Env::host();
        let mut app = configured();
        app.discovered_nics[0].device = "test-device".into();
        let (mut worker, engine) = worker(&env, &app, true, true);
        let mut service = test_owner(&app, engine);
        sim.implicit_odp(false);
        let mut run = |worker: &mut WorkerApplication| {
            worker.native_retry = worker.ctx().now();
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            for _ in 0..20 {
                worker.poll_native(&mut cx).unwrap();
                service.poll_budgeted(&mut cx, 64).unwrap();
            }
        };
        run(&mut worker);
        assert!(worker.rdma_withdrawn);
        publish_rails(&mut worker, &app, 2, Vec::new());
        sim.implicit_odp(true);
        run(&mut worker);
        assert!(worker.links.as_ref().unwrap().rail_ready(RailId(7)));
        assert!(!worker.rdma_withdrawn);
    }

    #[test]
    /// A failed activation backs off exponentially before the next attempt
    /// instead of retrying every second, and a success resets the backoff.
    fn failed_activation_backs_off_and_success_resets() {
        use rdma_verbs::simulation::Device;
        use rdma_verbs::simulation::Simulation;
        let sim = Simulation::new()
            .with_devices(vec![Device::new("new", [2; 16])])
            .unwrap();
        let _devices = sim.enter();
        let env = racer_io::Env::host();
        let app = configured();
        let (mut worker, engine) = worker(&env, &app, true, true);
        let mut service = test_owner(&app, engine);
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        let publication = racer_wire::control::Publication {
            pools: Vec::new(),
            schema_version: racer_wire::control::SCHEMA_VERSION,
            cluster: app.config.cluster.clone(),
            sequence: PublicationSequence(2),
            membership_version: MembershipVersion(2),
            members: vec![racer_wire::control::Member {
                node: app.config.node.clone(),
                shares: std::num::NonZeroU32::new(1).unwrap(),
                peer_endpoint: "127.0.0.1:7443".into(),
                rails: vec![racer_wire::control::RailMapping {
                    device: "new".into(),
                    port: 1,
                    gid: None,
                    rail: RailId(7),
                    numa_node: None,
                }],
                site: "site1".into(),
            }],
            caches: vec![],
            page_size: racer_wire::object::PageSize::DEFAULT,
        }
        .with_default_pool();
        worker.snapshots.apply(publication).unwrap();
        app.node.inventory.update(crate::rdma::inventory()).unwrap();
        // Without implicit ODP every activation fails.
        sim.implicit_odp(false);
        let mut attempt = |worker: &mut WorkerApplication| {
            worker.native_retry = racer_io::Clock::host().now();
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            for _ in 0..20 {
                worker.poll_native(&mut cx).unwrap();
                service.poll_budgeted(&mut cx, 8).unwrap();
                if worker.native_task.is_none() && worker.native_retry > worker.ctx().now() {
                    break;
                }
            }
        };
        for failures in 1..=3u32 {
            attempt(&mut worker);
            assert_eq!(worker.native_failures, failures);
            let wait = worker.native_retry - worker.ctx().now();
            assert!(wait > activation_retry_delay(failures) - Duration::from_millis(500));
            assert!(wait <= activation_retry_delay(failures));
            // Polling again before the retry time starts nothing.
            worker.poll_native(&mut cx).unwrap();
            assert!(worker.native_task.is_none(), "retried before the backoff");
        }
        assert!(worker.actual_rails.is_empty());
        sim.implicit_odp(true);
        attempt(&mut worker);
        assert!(worker.links.as_ref().unwrap().rail_ready(RailId(7)));
        assert_eq!(worker.native_failures, 0);
    }

    #[test]
    fn duplicate_discovery_cannot_activate_a_binding() {
        let mut app = configured();
        app.discovered_nics.push(app.discovered_nics[0].clone());
        assert!(
            crate::rdma::select_worker(&app.discovered_nics[..1], &app.discovered_nics, 0, None, 1)
                .is_empty()
        );
    }

    #[test]
    fn local_configuration_does_not_enable_unaligned_or_unmapped_membership() {
        let env = racer_io::Env::host();
        for (aligned, mapped, published_rails) in [
            (false, true, true),
            (true, false, true),
            (true, true, false),
        ] {
            let mut app = configured();
            if !mapped {
                app.discovered_nics.clear();
            }
            let (mut worker, _engine) = worker(&env, &app, aligned, published_rails);
            futures::executor::block_on(worker.activate_native(
                &scope(&racer_io::Clock::host(), Duration::from_secs(1)).unwrap(),
            ))
            .unwrap();
            assert!(worker.actual_rails.is_empty());
            assert!(!worker.links.as_ref().unwrap().rail_ready(RailId(7)));
            assert_eq!(worker.runtime.admission.used(ResourceClass::Registered), 0);
        }
    }

    #[test]
    fn local_configuration_cannot_replace_missing_local_membership() {
        let env = racer_io::Env::host();
        let app = configured();
        let (mut worker, _engine) = worker(&env, &app, true, true);
        worker
            .snapshots
            .apply(
                Publication {
                    pools: Vec::new(),
                    schema_version: racer_wire::control::SCHEMA_VERSION,
                    cluster: app.config.cluster.clone(),
                    sequence: PublicationSequence(2),
                    membership_version: MembershipVersion(2),
                    members: vec![],
                    caches: vec![],
                    page_size: racer_wire::object::PageSize::DEFAULT,
                }
                .with_default_pool(),
            )
            .unwrap();
        // Not a member yet (a new node joins only once it serves): startup continues without native rails, and local
        // configuration does not stand in for the missing membership.
        assert_eq!(
            futures::executor::block_on(worker.activate_native(
                &scope(&racer_io::Clock::host(), Duration::from_secs(1)).unwrap()
            )),
            Ok(())
        );
        assert_eq!(
            worker.native_publication(),
            Err(Error::IncompatibleMembership)
        );
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        worker.native_retry = racer_io::Clock::host().now();
        worker.poll_native(&mut cx).unwrap();
        assert!(worker.native_task.is_none());
        assert!(worker.actual_rails.is_empty());
        assert!(!worker.links.as_ref().unwrap().rail_ready(RailId(7)));
        assert_eq!(worker.runtime.admission.used(ResourceClass::Registered), 0);
    }

    /// Readiness needs this node in its own membership: a new node is
    /// listed only after it serves.
    #[test]
    fn membership_is_usable_only_when_it_lists_the_node() {
        let env = racer_io::Env::host();
        let app = configured();
        let (mut worker, _engine) = worker(&env, &app, true, true);
        let listed = worker.snapshots.current().unwrap();
        worker.snapshot_sequence = Some(listed.sequence);
        assert!(worker.membership_usable(Some(&listed)));
        assert!(!worker.membership_usable(None));
        worker
            .snapshots
            .apply(
                Publication {
                    pools: Vec::new(),
                    schema_version: racer_wire::control::SCHEMA_VERSION,
                    cluster: app.config.cluster.clone(),
                    sequence: PublicationSequence(2),
                    membership_version: MembershipVersion(2),
                    members: vec![],
                    caches: vec![],
                    page_size: racer_wire::object::PageSize::DEFAULT,
                }
                .with_default_pool(),
            )
            .unwrap();
        let unlisted = worker.snapshots.current().unwrap();
        worker.snapshot_sequence = Some(unlisted.sequence);
        assert!(!worker.membership_usable(Some(&unlisted)));
        // A snapshot this worker has not applied is not usable either.
        worker.snapshot_sequence = Some(unlisted.sequence);
        assert!(!worker.membership_usable(Some(&listed)));
    }

    /// A node admitted after it started activates its rails from the
    /// publication that first lists it.
    #[test]
    fn native_activates_once_membership_lists_the_node() {
        use rdma_verbs::simulation::Device;
        use rdma_verbs::simulation::Simulation;
        let sim = Simulation::new();
        let simulated = sim
            .with_devices(vec![Device::new("test-device", [1; 16])])
            .unwrap();
        let _devices = simulated.enter();
        let env = racer_io::Env::host();
        let mut app = configured();
        app.discovered_nics[0].device = "test-device".into();
        let (mut worker, engine) = worker(&env, &app, true, true);
        let listed = worker.snapshots.current().unwrap().membership.clone();
        worker
            .snapshots
            .apply(
                Publication {
                    pools: Vec::new(),
                    schema_version: racer_wire::control::SCHEMA_VERSION,
                    cluster: app.config.cluster.clone(),
                    sequence: PublicationSequence(2),
                    membership_version: MembershipVersion(2),
                    members: vec![],
                    caches: vec![],
                    page_size: racer_wire::object::PageSize::DEFAULT,
                }
                .with_default_pool(),
            )
            .unwrap();
        assert_eq!(
            futures::executor::block_on(worker.activate_native(
                &scope(&racer_io::Clock::host(), Duration::from_secs(1)).unwrap()
            )),
            Ok(())
        );
        let mut service = test_owner(&app, engine);
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        worker.native_retry = racer_io::Clock::host().now();
        worker.poll_native(&mut cx).unwrap();
        assert!(!worker.links.as_ref().unwrap().rail_ready(RailId(7)));
        let member = listed.member(&app.config.node).unwrap().clone();
        worker
            .snapshots
            .apply(
                Publication {
                    pools: Vec::new(),
                    schema_version: racer_wire::control::SCHEMA_VERSION,
                    cluster: app.config.cluster.clone(),
                    sequence: PublicationSequence(3),
                    membership_version: MembershipVersion(3),
                    members: vec![racer_wire::control::Member {
                        node: app.config.node.clone(),
                        shares: member.shares,
                        peer_endpoint: member.peer_endpoint.clone(),
                        rails: app.discovered_nics.clone(),
                        site: member.site.clone(),
                    }],
                    caches: vec![],
                    page_size: racer_wire::object::PageSize::DEFAULT,
                }
                .with_default_pool(),
            )
            .unwrap();
        worker.native_retry = racer_io::Clock::host().now();
        for _ in 0..20 {
            worker.poll_native(&mut cx).unwrap();
            service
                .poll_budgeted(&mut Context::from_waker(futures::task::noop_waker_ref()), 8)
                .unwrap();
        }
        assert!(worker.links.as_ref().unwrap().rail_ready(RailId(7)));
    }

    fn activation_falls_back(env: &racer_io::Env) {
        let app = configured();
        assert_eq!(app.discovered_nics[0].device, "missing");
        let (mut worker, engine) = worker(env, &app, true, true);
        let admission = worker.runtime.admission.clone();
        let startup = scope(&env.ctx.clock, Duration::from_secs(10)).unwrap();
        let mut activation = Box::pin(worker.activate_native(&startup));
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        assert!(
            activation.as_mut().poll(&mut cx).is_pending(),
            "explicit configuration must reach paired native provisioning"
        );
        assert!(admission.used(ResourceClass::Registered) > 0);
        std::thread::scope(|threads| {
            threads
                .spawn(|| {
                    let mut service = test_owner(&app, engine);
                    service
                        .poll_budgeted(&mut Context::from_waker(futures::task::noop_waker_ref()), 1)
                        .unwrap();
                })
                .join()
                .unwrap();
        });
        assert!(matches!(
            activation.as_mut().poll(&mut cx),
            Poll::Ready(Ok(()))
        ));
        drop(activation);
        assert!(worker.actual_rails.is_empty());
        assert!(!worker.links.as_ref().unwrap().rail_ready(RailId(7)));
        assert!(!worker.links.as_ref().unwrap().rail_ready(RailId(7)));
        assert_eq!(admission.used(ResourceClass::Registered), 0);
    }

    #[cfg(not(feature = "rdma"))]
    #[test]
    fn configured_activation_without_loader_keeps_http_available() {
        let env = racer_io::Env::host();
        activation_falls_back(&env);
    }

    #[cfg(feature = "rdma")]
    #[test]
    #[ignore = "requires actual ABI v1 libibverbs adapter and zero usable type-2B ports"]
    fn configured_native_no_device_activation_keeps_http_available() {
        // Enforce the environment before testing app fallback; missing libraries
        // or a hardware-capable host must not masquerade as no-device coverage.
        unsafe {
            let library = libc::dlopen(
                c"librdma_verbs.so.1".as_ptr(),
                libc::RTLD_NOW | libc::RTLD_LOCAL,
            );
            assert!(!library.is_null(), "real native library must load");
            let abi = libc::dlsym(library, c"rdma_verbs_abi".as_ptr());
            let discover = libc::dlsym(library, c"rdma_verbs_discover".as_ptr());
            assert!(!abi.is_null() && !discover.is_null());
            let abi: unsafe extern "C" fn() -> u32 = std::mem::transmute(abi);
            let discover: unsafe extern "C" fn(*mut libc::c_void, u32) -> libc::c_int =
                std::mem::transmute(discover);
            assert_eq!(abi(), 1);
            assert_eq!(
                discover(std::ptr::null_mut(), 0),
                0,
                "test requires zero usable ports"
            );
            assert_eq!(libc::dlclose(library), 0);
        }
        activation_falls_back(&racer_io::Env::host());
    }
}
