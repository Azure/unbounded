//! Native lifecycle endpoints remain per I/O shard, even with shared crypto threads.
use super::*;
use crate::rdma::WithNative;
use crate::runtime::collections::HashMap;
use rdma_verbs::{IoPort, NativePort};

#[derive(Default)]
pub(super) struct NativePairs {
    ports: Mutex<HashMap<WorkerId, NativePair>>,
    numa: Mutex<HashMap<WorkerId, NativePlacement>>,
}
struct NativePair {
    io: Option<IoPort>,
    crypto: Option<NativePort>,
}

/// NUMA locality of the crypto role that first-touches registered allocations.
#[derive(Clone, Copy)]
pub(super) struct NativePlacement {
    pub(super) numa_node: Option<usize>,
}

pub(super) fn slot_count(limits: &Limits) -> Result<usize> {
    let charge = crate::rdma::native_slot_charge(crate::rdma::MAX_CIPHERTEXT)?;
    Ok((limits.registered_bytes.get() / charge)
        .min(limits.queue_entries.get())
        .min(256))
}

impl NativePairs {
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
    pub(super) fn prepare(
        &self,
        workers: impl Iterator<Item = WorkerId>,
        limits: &Limits,
    ) -> Result<()> {
        let slots = slot_count(limits)?;
        // Insufficient optional native capacity uses HTTP without inventing quota.
        if slots == 0 {
            return Ok(());
        }
        let mut ports = self.ports.lock().map_err(|_| Error::Unavailable)?;
        for worker in workers {
            let (io, native) = rdma_verbs::pair(slots)?;
            ports.insert(
                worker,
                NativePair {
                    io: Some(io),
                    crypto: Some(native),
                },
            );
        }
        Ok(())
    }
    pub(super) fn io(&self, worker: WorkerId) -> Result<Option<IoPort>> {
        Ok(self
            .ports
            .lock()
            .map_err(|_| Error::Unavailable)?
            .get_mut(&worker)
            .and_then(|pair| pair.io.take()))
    }
    pub(super) fn crypto(
        &self,
        worker: WorkerId,
        engine: PageCryptoEngine,
    ) -> Result<Box<dyn CryptoService>> {
        let native = self
            .ports
            .lock()
            .map_err(|_| Error::Unavailable)?
            .get_mut(&worker)
            .and_then(|pair| pair.crypto.take());
        Ok(match native {
            Some(native) => Box::new(WithNative::new(engine, native)),
            None => Box::new(engine),
        })
    }
}
impl Application {
    pub fn discovered_nics(&self) -> &[crate::topology::rails::RailMapping] {
        &self.discovered_nics
    }
}
impl WorkerApplication {
    /// Inventory withdrawal/GID changes invalidate in-progress activation before
    /// it can publish readiness. Existing generations drain on their native owner.
    pub(super) fn refresh_inventory(&mut self) -> Result<()> {
        let snapshot = self.node.inventory.snapshot()?;
        if snapshot.generation != self.inventory_generation {
            self.native_task.take();
            if let Some(devices) = &self.devices {
                devices.close();
            }
            self.actual_rails.clear();
            self.discovered_nics = snapshot.nics;
            self.inventory_generation = snapshot.generation;
            self.native_retry = uring_runtime::environment::now();
        }
        Ok(())
    }
    pub(super) fn native_publication(&self) -> Result<Vec<crate::topology::rails::RailMapping>> {
        let snapshot = self.snapshots.current()?;
        let member = snapshot.membership.member(self.keys.node())?;
        let capacity = self.devices.as_ref().map_or(0, |d| d.capacity());
        Ok(crate::rdma::discovery::select_worker(
            &member.rails,
            &self.discovered_nics,
            usize::from(self.worker.0),
            self.native_numa.and_then(|p| p.numa_node),
            capacity,
        ))
    }
    pub(super) fn poll_native(&mut self, cx: &mut Context<'_>) -> Result<()> {
        if self.stopping {
            self.native_task.take();
            return Ok(());
        }
        self.refresh_inventory()?;
        if let Some(task) = self.native_task.as_mut() {
            if let Poll::Ready(result) = task.as_mut().poll(cx) {
                self.native_task = None;
                self.native_retry = uring_runtime::environment::now() + Duration::from_secs(1);
                match result {
                    Ok(actual) => self.actual_rails = actual,
                    Err(_) => {
                        if let Some(devices) = &self.devices {
                            devices.close();
                        }
                    }
                }
            }
        }
        if self.stopping
            || self.native_task.is_some()
            || !self.actual_rails.is_empty()
            || uring_runtime::environment::now() < self.native_retry
        {
            return Ok(());
        }
        let Some(devices) = self.devices.clone() else {
            return Ok(());
        };
        let publication = self.native_publication()?;
        if publication.is_empty() {
            return Ok(());
        }
        let admission = self.runtime.admission.clone();
        let turn = scope(Duration::from_secs(5))?;
        self.native_task = Some(Box::pin(async move {
            devices
                .activate(publication, &admission, crate::rdma::MAX_CIPHERTEXT, &turn)
                .await
        }));
        cx.waker().wake_by_ref();
        Ok(())
    }
    pub(super) async fn activate_native(&mut self, startup: &RequestScope) -> Result<()> {
        self.refresh_inventory()?;
        let Some(devices) = &self.devices else {
            return Ok(());
        };
        let publication = self.native_publication()?;
        if publication.is_empty() {
            return Ok(());
        }
        match devices
            .activate(
                publication,
                &self.runtime.admission,
                crate::rdma::MAX_CIPHERTEXT,
                startup,
            )
            .await
        {
            Ok(actual) => {
                self.actual_rails = actual;
            }
            Err(Error::Cancelled | Error::DeadlineExceeded) => {
                startup.check()?;
            }
            Err(_) => {
                devices.close();
            }
        }
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::{
        control::state::Publication,
        model::{MembershipVersion, ResourceClass},
        runtime::crypto::{self, CryptoClient},
        topology::{membership::Member, rails::RailId},
    };
    use racer_control_wire::PublicationSequence;
    use std::num::NonZeroUsize;

    #[test]
    fn shared_inventory_recovers_new_hardware_and_revokes_removed_or_changed_ports() {
        use rdma_verbs::simulation::{Device, Simulation};
        let sim = Simulation::new()
            .with_devices(vec![
                Device::new("missing", [1; 16]),
                Device::new("new", [2; 16]),
            ])
            .unwrap();
        let _environment = sim.enter();
        let app = configured();
        let (mut worker, engine) = worker(&app, true, true);
        let mut service = app.build_crypto(WorkerId(0), engine).unwrap();
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        let mut publication = crate::control::state::Publication {
            schema_version: 1,
            cluster: app.config.cluster.clone(),
            sequence: PublicationSequence(2),
            membership_version: MembershipVersion(2),
            members: vec![Member {
                node: app.config.node.clone(),
                shares: std::num::NonZeroU32::new(1).unwrap(),
                peer_endpoint: "127.0.0.1:7443".into(),
                rails: vec![crate::topology::rails::RailMapping {
                    device: "new".into(),
                    port: 1,
                    gid: None,
                    rail: RailId(7),
                    numa_node: None,
                }],
                site: "site1".into(),
            }],
            caches: vec![],
        };
        worker.snapshots.publish(publication.clone()).unwrap();
        app.node.inventory.update(vec![]).unwrap();
        worker.poll_native(&mut cx).unwrap();
        assert!(worker.native_publication().unwrap().is_empty());
        let fresh = crate::rdma::discovery::inventory();
        app.node.inventory.update(fresh.clone()).unwrap();
        for _ in 0..20 {
            worker.poll_native(&mut cx).unwrap();
            service.poll_budgeted(8).unwrap();
        }
        assert_eq!(worker.actual_rails[0].device, "new");
        assert!(worker.devices.as_ref().unwrap().ready(RailId(7)));
        // No membership change is needed for local discovery withdrawal to revoke.
        app.node.inventory.update(vec![]).unwrap();
        worker.refresh_inventory().unwrap();
        assert!(!worker.devices.as_ref().unwrap().ready(RailId(7)));
        service.poll_budgeted(256).unwrap();
        assert_eq!(worker.runtime.admission.used(ResourceClass::Registered), 0);
        app.node.inventory.update(fresh.clone()).unwrap();
        for _ in 0..20 {
            worker.poll_native(&mut cx).unwrap();
            service.poll_budgeted(8).unwrap();
        }
        assert!(worker.devices.as_ref().unwrap().ready(RailId(7)));
        let mut changed = fresh;
        changed.iter_mut().find(|n| n.device == "new").unwrap().gid = Some([3; 16]);
        app.node.inventory.update(changed).unwrap();
        worker.refresh_inventory().unwrap();
        assert!(!worker.devices.as_ref().unwrap().ready(RailId(7)));
        service.poll_budgeted(256).unwrap();
        assert_eq!(worker.runtime.admission.used(ResourceClass::Registered), 0);
        // Published GID must still match refreshed discovery, never bypass it.
        publication.sequence.0 += 1;
        publication.membership_version.0 += 1;
        publication.members[0].rails[0].gid = Some([2; 16]);
        worker.snapshots.publish(publication).unwrap();
        assert!(worker.native_publication().unwrap().is_empty());
        drop(service);
        assert_eq!(sim.live_resources(), 0);
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
            13 + usize::from(values.contains_key("RACER_MAX_THREADS"))
        );
        let mut config = Config::from_lookup(|name| {
            Ok(values
                .get(name)
                .map(|v| (*v).to_owned())
                .or_else(|| match name {
                    "RACER_CLUSTER_ID" => Some("00000000-0000-4000-8000-000000000001".into()),
                    "RACER_CONTROL_ENDPOINT" => Some("https://control.example".into()),
                    _ => None,
                }))
        })
        .unwrap();
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
        assert_eq!(config.slab_bytes, 1024 * 1024 * 1024);
        assert_eq!(
            config.limits.request_context_bytes,
            default_config(false).limits.request_context_bytes
        );
        let mut plan = four_pair_plan(&config);
        let limits = size_workers(&config.limits, &mut plan, config.enable_rdma).unwrap();
        assert_eq!(plan.pairs.len(), 5);
        let admission = flow_control::Quotas::new(AdmissionPolicy::new(limits.clone()));
        let page = crate::model::PAGE_BYTES as usize;
        for (class, floor) in [
            (ResourceClass::Plaintext, 3 * page),
            (
                ResourceClass::Ciphertext,
                3 * (page + 16) + crate::store::format::MAX_HEADER_BYTES,
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
        assert_eq!(limits.plaintext_bytes.get(), 256 * 1024 * 1024 / 5);
        assert_eq!(limits.ciphertext_bytes.get(), 256 * 1024 * 1024 / 5);
        assert_eq!(limits.dirty_bytes.get(), 128 * 1024 * 1024 / 5);
        assert_eq!(limits.request_context_bytes.get(), 64 * 1024 * 1024 / 5);
        assert_eq!(admission.used(ResourceClass::Registered), 0);
        // Reducing thread cap or quotas keeps local groups and re-partitions the
        // node budget. An impossible byte floor must fail rather than deadlock.
        let mut too_small = config.limits.clone();
        too_small.plaintext_bytes = NonZeroUsize::new(3 * page - 1).unwrap();
        assert!(size_workers(&too_small, &mut plan, false).is_err());
    }

    fn four_pair_plan(config: &Config) -> AffinityPlan {
        use uring_runtime::affinity::{CpuLocation, EffectiveTopology};
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
        use uring_runtime::affinity::{CpuLocation, EffectiveTopology};
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
                    "RACER_CONNECTIONS_PER_NEIGHBOR" => Some("16".into()),
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
            let floor = 3 * crate::model::PAGE_BYTES as usize;
            config.limits.plaintext_bytes = NonZeroUsize::new(2 * floor).unwrap();
            let mut plan = make_plan(&config);
            let limits = size_workers(&config.limits, &mut plan, false).unwrap();
            assert_eq!(plan.pairs.len(), 2);
            assert_eq!(limits.plaintext_bytes.get(), floor);
            assert_eq!(limits.relay_transfers.get(), 128);
            assert_eq!(plan.crypto_groups(), vec![vec![0, 1]]);
            assert!(
                plan.pairs
                    .iter()
                    .all(|pair| pair.io.numa_node == pair.crypto.numa_node)
            );
            assert_eq!(
                plan.pairs.iter().map(|p| p.worker).collect::<Vec<_>>(),
                vec![WorkerId(0), WorkerId(1)]
            );
            config.limits.plaintext_bytes = NonZeroUsize::new(floor - 1).unwrap();
            assert!(size_workers(&config.limits, &mut make_plan(&config), false).is_err());
        }
    }

    #[test]
    fn resource_shortage_reduces_and_regroups_uneven_numa_workers() {
        use uring_runtime::affinity::{CpuLocation, EffectiveTopology};
        let mut config = default_config(false);
        let floor = 3 * crate::model::PAGE_BYTES as usize;
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
            let limits = size_workers(&config.limits, &mut plan, false).unwrap();
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
    fn native_endpoints_remain_per_io_with_shared_crypto_placement() {
        let config = default_config(true);
        let mut plan = four_pair_plan(&config);
        let limits = size_workers(&config.limits, &mut plan, true).unwrap();
        assert!(plan.crypto_groups().len() < plan.pairs.len());
        let native = NativePairs::default();
        native.place(&plan).unwrap();
        native
            .prepare(plan.pairs.iter().map(|pair| pair.worker), &limits)
            .unwrap();
        for pair in &plan.pairs {
            assert_eq!(
                native
                    .numa(pair.worker)
                    .unwrap()
                    .map(|placement| placement.numa_node),
                Some(pair.crypto.numa_node)
            );
            assert!(native.io(pair.worker).unwrap().is_some());
            assert!(native.io(pair.worker).unwrap().is_none());
        }
    }

    #[test]
    fn default_worker_sizing_funds_native_slots_within_node_budgets() {
        let config = default_config(true);
        assert_eq!(config.limits.registered_bytes.get(), 128 * 1024 * 1024);
        let mut plan = four_pair_plan(&config);
        let limits = size_workers(&config.limits, &mut plan, true).unwrap();
        assert_eq!(plan.pairs.len(), 3);
        assert_eq!(plan.max_threads, usize::MAX);
        for (worker, pair) in plan.pairs.iter().enumerate() {
            assert_eq!(pair.worker, WorkerId(worker as u16));
        }
        for (partition, node) in [
            (limits.plaintext_bytes, config.limits.plaintext_bytes),
            (limits.ciphertext_bytes, config.limits.ciphertext_bytes),
            (limits.dirty_bytes, config.limits.dirty_bytes),
            (limits.registered_bytes, config.limits.registered_bytes),
            (
                limits.request_context_bytes,
                config.limits.request_context_bytes,
            ),
            (limits.flights, config.limits.flights),
            (limits.queue_entries, config.limits.queue_entries),
            (limits.client_connections, config.limits.client_connections),
            (limits.pipes, config.limits.pipes),
            (limits.cached_rankings, config.limits.cached_rankings),
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

        let native = NativePairs::default();
        native
            .prepare(plan.pairs.iter().map(|p| p.worker), &limits)
            .unwrap();
        let mut total = 0;
        for pair in &plan.pairs {
            let port = native
                .io(pair.worker)
                .unwrap()
                .expect("funded native worker");
            assert_eq!(port.capacity(), 1);
            let devices = Devices::new();
            devices.attach(port).unwrap();
            let admission = flow_control::Quotas::new(AdmissionPolicy::new(limits.clone()));
            let startup = scope(Duration::from_secs(10)).unwrap();
            let mut activation =
                devices.activate(vec![], &admission, crate::rdma::MAX_CIPHERTEXT, &startup);
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            assert!(activation.as_mut().poll(&mut cx).is_pending());
            let used = admission.used(ResourceClass::Registered);
            assert_eq!(used, 32 * 1024 * 1024 + 8192);
            assert!(used <= limits.registered_bytes.get());
            total += used;
        }
        assert!(total <= config.limits.registered_bytes.get());
    }

    #[test]
    fn native_worker_sizing_observes_exact_slot_boundaries() {
        let mut config = default_config(true);
        let charge = 32 * 1024 * 1024 + 8192;
        for (budget, expected) in [
            (1, 0),
            (16 * 1024 * 1024 + 16, 0),
            (charge - 1, 0),
            (charge, 1),
            (charge + 1, 1),
            (2 * charge - 1, 1),
            (2 * charge, 2),
            (3 * charge - 1, 2),
            (3 * charge, 3),
            (4 * charge - 1, 3),
            (4 * charge, 4),
            (4 * charge + 3, 4),
        ] {
            config.limits.registered_bytes = NonZeroUsize::new(budget).unwrap();
            let mut plan = four_pair_plan(&config);
            let result = size_workers(&config.limits, &mut plan, true);
            if expected == 0 {
                assert!(
                    matches!(result, Err(Error::InvalidConfiguration)),
                    "budget={budget}"
                );
            } else {
                let limits = result.unwrap();
                assert_eq!(plan.pairs.len(), expected, "budget={budget}");
                assert!(slot_count(&limits).unwrap() >= 1);
                assert!(limits.registered_bytes.get() * expected <= budget);
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
                let limits = size_workers(&config.limits, &mut plan, rdma).unwrap();
                assert_eq!(plan.pairs.len(), 2);
                assert_eq!(limits.pipes.get(), 1);
            }
            config.limits.pipes = NonZeroUsize::new(16).unwrap();
        }
    }

    #[test]
    fn native_preparation_preserves_slot_caps_and_unfunded_fallback() {
        let mut limits = default_config(true).limits;
        let charge = 32 * 1024 * 1024 + 8192;
        for (budget, queue, expected) in [
            (charge - 1, 256, 0),
            (charge, 256, 1),
            (2 * charge - 1, 256, 1),
            (3 * charge, 2, 2),
            (257 * charge, 512, 256),
        ] {
            limits.registered_bytes = NonZeroUsize::new(budget).unwrap();
            limits.queue_entries = NonZeroUsize::new(queue).unwrap();
            let native = NativePairs::default();
            native
                .prepare(std::iter::once(WorkerId(0)), &limits)
                .unwrap();
            let port = native.io(WorkerId(0)).unwrap();
            assert_eq!(port.as_ref().map_or(0, IoPort::capacity), expected);
        }
    }

    #[test]
    fn native_activation_exhaustion_fails_closed_and_rolls_back_partial_quota() {
        let mut limits = default_config(true).limits;
        limits.registered_bytes = NonZeroUsize::new(2 * (32 * 1024 * 1024 + 8192)).unwrap();
        let native = NativePairs::default();
        native
            .prepare(std::iter::once(WorkerId(0)), &limits)
            .unwrap();
        let port = native.io(WorkerId(0)).unwrap().unwrap();
        assert_eq!(port.capacity(), 2);
        let devices = Devices::new();
        devices.attach(port).unwrap();
        let admission = flow_control::Quotas::new(AdmissionPolicy::new(limits));
        let held = admission
            .reserve(None, ResourceClass::Registered, 1)
            .unwrap();
        let startup = scope(Duration::from_secs(10)).unwrap();
        assert!(matches!(
            futures::executor::block_on(devices.activate(
                vec![],
                &admission,
                crate::rdma::MAX_CIPHERTEXT,
                &startup,
            )),
            Err(Error::Overloaded)
        ));
        assert_eq!(admission.used(ResourceClass::Registered), 1);
        assert!(!devices.ready(RailId(0)));
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
        app.discovered_nics = vec![crate::topology::rails::RailMapping {
            device: "missing".into(),
            port: 1,
            gid: Some([1; 16]),
            rail: RailId(7),
            numa_node: None,
        }];
        app
    }

    fn worker(
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
        let (io, engine) = crypto::pair(WorkerId(0), 0, app.limits.queue_entries);
        let runtime = WorkerRuntime {
            reactor: Rc::new(Reactor::new(admission.clone())),
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
            .publish(Publication {
                schema_version: 1,
                cluster: app.config.cluster.clone(),
                sequence: PublicationSequence(1),
                membership_version: MembershipVersion(1),
                members: vec![Member {
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
            })
            .unwrap();
        (worker, CryptoRuntime { port: engine })
    }

    #[test]
    fn native_worker_selects_only_funded_local_rails_and_recovers() {
        use rdma_verbs::simulation::{Device, Simulation};
        let sim = Simulation::new();
        let simulated = sim
            .with_devices(vec![Device::new("test-device", [1; 16])])
            .unwrap();
        let _environment = simulated.enter();
        let mut app = configured();
        app.discovered_nics[0].device = "test-device".into();
        let (mut worker, engine) = worker(&app, true, true);
        worker.native_numa = Some(NativePlacement { numa_node: None });
        assert_eq!(worker.native_publication().unwrap().len(), 1);
        worker.native_numa = None;
        assert_eq!(worker.native_publication().unwrap().len(), 1);
        let mut service = app.build_crypto(WorkerId(0), engine).unwrap();
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        for _ in 0..20 {
            worker.poll_native(&mut cx).unwrap();
            service.poll_budgeted(8).unwrap();
        }
        assert!(worker.devices.as_ref().unwrap().ready(RailId(7)));
        let charged = worker.runtime.admission.used(ResourceClass::Registered);
        assert!(charged > 0);
        worker.devices.as_ref().unwrap().close();
        worker.actual_rails.clear();
        service.poll_budgeted(256).unwrap();
        assert_eq!(worker.runtime.admission.used(ResourceClass::Registered), 0);
        worker.native_retry = uring_runtime::environment::now();
        for _ in 0..20 {
            worker.poll_native(&mut cx).unwrap();
            service.poll_budgeted(8).unwrap();
        }
        assert!(worker.devices.as_ref().unwrap().ready(RailId(7)));
        assert_eq!(
            worker.runtime.admission.used(ResourceClass::Registered),
            charged
        );
        worker
            .snapshots
            .publish(Publication {
                schema_version: 1,
                cluster: app.config.cluster.clone(),
                sequence: PublicationSequence(2),
                membership_version: MembershipVersion(2),
                members: vec![Member {
                    node: app.config.node.clone(),
                    shares: std::num::NonZeroU32::new(1).unwrap(),
                    peer_endpoint: "127.0.0.1:7443".into(),
                    rails: vec![],
                    site: "site1".into(),
                }],
                caches: vec![],
            })
            .unwrap();
        worker
            .refresh_snapshot(&scope(Duration::from_secs(1)).unwrap())
            .unwrap();
        assert!(worker.actual_rails.is_empty());
        assert!(!worker.devices.as_ref().unwrap().ready(RailId(7)));
        service.poll_budgeted(256).unwrap();
        assert_eq!(worker.runtime.admission.used(ResourceClass::Registered), 0);
        drop(service);
        assert_eq!(sim.live_resources(), 0);
    }
    #[test]
    fn duplicate_discovery_cannot_activate_a_binding() {
        let mut app = configured();
        app.discovered_nics.push(app.discovered_nics[0].clone());
        assert!(
            crate::rdma::discovery::select_worker(
                &app.discovered_nics[..1],
                &app.discovered_nics,
                0,
                None,
                1
            )
            .is_empty()
        );
    }

    #[test]
    fn local_configuration_does_not_enable_unaligned_or_unmapped_membership() {
        for (aligned, mapped, published_rails) in [
            (false, true, true),
            (true, false, true),
            (true, true, false),
        ] {
            let mut app = configured();
            if !mapped {
                app.discovered_nics.clear();
            }
            let (mut worker, _engine) = worker(&app, aligned, published_rails);
            futures::executor::block_on(
                worker.activate_native(&scope(Duration::from_secs(1)).unwrap()),
            )
            .unwrap();
            assert!(worker.actual_rails.is_empty());
            assert!(!worker.rdma.as_ref().unwrap().ready(RailId(7)));
            assert_eq!(worker.runtime.admission.used(ResourceClass::Registered), 0);
        }
    }

    #[test]
    fn local_configuration_cannot_replace_missing_local_membership() {
        let app = configured();
        let (mut worker, _engine) = worker(&app, true, true);
        worker
            .snapshots
            .publish(Publication {
                schema_version: 1,
                cluster: app.config.cluster.clone(),
                sequence: PublicationSequence(2),
                membership_version: MembershipVersion(2),
                members: vec![],
                caches: vec![],
            })
            .unwrap();
        assert_eq!(
            futures::executor::block_on(
                worker.activate_native(&scope(Duration::from_secs(1)).unwrap())
            ),
            Err(Error::IncompatibleMembership)
        );
        assert!(worker.actual_rails.is_empty());
        assert!(!worker.devices.as_ref().unwrap().ready(RailId(7)));
        assert_eq!(worker.runtime.admission.used(ResourceClass::Registered), 0);
    }

    fn activation_falls_back() {
        let app = configured();
        assert_eq!(app.discovered_nics[0].device, "missing");
        let (mut worker, engine) = worker(&app, true, true);
        let admission = worker.runtime.admission.clone();
        let startup = scope(Duration::from_secs(10)).unwrap();
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
                    let mut service = app.build_crypto(WorkerId(0), engine).unwrap();
                    service.poll_budgeted(1).unwrap();
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
        assert!(!worker.devices.as_ref().unwrap().ready(RailId(7)));
        assert!(!worker.rdma.as_ref().unwrap().ready(RailId(7)));
        assert_eq!(admission.used(ResourceClass::Registered), 0);
    }

    #[cfg(not(feature = "rdma"))]
    #[test]
    fn configured_activation_without_loader_keeps_http_available() {
        activation_falls_back();
    }

    #[cfg(feature = "rdma")]
    #[test]
    #[ignore = "requires actual ABI v2 libibverbs adapter and zero usable type-2B ports"]
    fn configured_native_no_device_activation_keeps_http_available() {
        // Enforce the environment before testing app fallback; missing libraries
        // or a hardware-capable host must not masquerade as no-device coverage.
        unsafe {
            let library = libc::dlopen(
                c"libracer_rdma.so.1".as_ptr(),
                libc::RTLD_NOW | libc::RTLD_LOCAL,
            );
            assert!(!library.is_null(), "real native library must load");
            let abi = libc::dlsym(library, c"racer_rdma_abi".as_ptr());
            let discover = libc::dlsym(library, c"racer_rdma_discover".as_ptr());
            assert!(!abi.is_null() && !discover.is_null());
            let abi: unsafe extern "C" fn() -> u32 = std::mem::transmute(abi);
            let discover: unsafe extern "C" fn(*mut libc::c_void, u32) -> libc::c_int =
                std::mem::transmute(discover);
            assert_eq!(abi(), 2);
            assert_eq!(
                discover(std::ptr::null_mut(), 0),
                0,
                "test requires zero usable ports"
            );
            assert_eq!(libc::dlclose(library), 0);
        }
        activation_falls_back();
    }
}
