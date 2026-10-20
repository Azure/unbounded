//! One RDMA owner per node, hosted beside crypto on the thread paired with the
//! first I/O worker; every I/O worker gets a client (`designs/racer-rdma-sessions.md`,
//! "Threading").

use super::*;
use crate::rdma::WithOwner;
use crate::runtime::HashMap;

#[derive(Default)]
pub(super) struct NativeNode {
    clients: Mutex<HashMap<WorkerId, rdma_verbs::Client>>,
    owner: Mutex<Option<(WorkerId, rdma_verbs::OwnerPort)>>,
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
    /// Create the node owner and one client per worker. The first worker's
    /// crypto thread hosts the owner.
    pub(super) fn prepare(
        &self,
        workers: impl Iterator<Item = WorkerId>,
        limits: &Limits,
    ) -> Result<()> {
        // Insufficient optional native capacity uses HTTP without inventing quota.
        if buffer_count(limits) < 2 {
            return Ok(());
        }
        let workers: Vec<_> = workers.collect();
        let Some(&first) = workers.first() else {
            return Ok(());
        };
        let (clients, port) = rdma_verbs::node(workers.len())?;
        let mut map = self.clients.lock().map_err(|_| Error::Unavailable)?;
        map.extend(workers.into_iter().zip(clients));
        *self.owner.lock().map_err(|_| Error::Unavailable)? = Some((first, port));
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
    /// The worker whose paired crypto thread hosts the owner.
    pub(super) fn owner_worker(&self) -> Option<WorkerId> {
        *self
            .owner_worker
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
    }
    pub(super) fn link_node(&self) -> Arc<crate::rdma::LinkNode> {
        self.links.clone()
    }
    pub(super) fn crypto(
        &self,
        worker: WorkerId,
        engine: PageCryptoEngine,
    ) -> Result<Box<dyn racer_platform::group::Service<RequestScope>>> {
        let mut owner = self.owner.lock().map_err(|_| Error::Unavailable)?;
        Ok(match owner.take_if(|(hosted, _)| *hosted == worker) {
            // Build the owner here, on the native thread that polls it.
            Some((_, port)) => Box::new(WithOwner::with_host(engine, rdma_verbs::Owner::new(port))),
            None => Box::new(engine),
        })
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
            self.native_retry = racer_platform::environment::now();
        }
        Ok(())
    }
    /// The node's rails: one device per published rail, chosen near the owner.
    pub(super) fn native_publication(&self) -> Result<Vec<racer_control_wire::RailMapping>> {
        let snapshot = self.snapshots.current()?;
        let member = snapshot.membership.member(self.keys.node())?;
        let owner = self.node.native.owner_worker();
        let numa = match owner {
            Some(owner) => self.node.native.numa(owner)?.and_then(|p| p.numa_node),
            None => None,
        };
        Ok(crate::rdma::select_worker(
            &member.rails,
            &self.discovered_nics,
            0,
            numa,
            usize::from(self.links.is_some()),
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
        if let Some(task) = self.native_task.as_mut()
            && let Poll::Ready(result) = task.as_mut().poll(cx)
        {
            self.native_task = None;
            self.native_retry = racer_platform::environment::now() + Duration::from_secs(1);
            if result.is_err() {
                links.close();
            }
        }
        self.actual_rails = links.active();
        if !self.native_owner()
            || self.native_task.is_some()
            || !self.actual_rails.is_empty()
            || racer_platform::environment::now() < self.native_retry
        {
            return Ok(());
        }
        let publication = self.native_publication()?;
        if publication.is_empty() {
            return Ok(());
        }
        let admission = self.runtime.admission.clone();
        let registered = admission.limit(crate::admission::ResourceClass::Registered);
        let turn = scope(Duration::from_secs(5))?;
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
        let publication = self.native_publication()?;
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
            Err(Error::Cancelled | Error::DeadlineExceeded) => {
                startup.check()?;
            }
            Err(_) => {
                links.close();
            }
        }
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::admission::ResourceClass;
    use crate::security;
    use crate::security::CryptoClient;
    use racer_control_wire::MembershipVersion;
    use racer_control_wire::Publication;
    use racer_control_wire::PublicationSequence;
    use racer_control_wire::RailId;
    use std::num::NonZeroUsize;

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
        let _environment = sim.enter();
        let app = configured();
        let (mut worker, engine) = worker(&app, true, true);
        let mut service = app.build_crypto(WorkerId(0), engine).unwrap();
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        let mut publication = racer_control_wire::Publication {
            schema_version: 1,
            cluster: app.config.cluster.clone(),
            sequence: PublicationSequence(2),
            membership_version: MembershipVersion(2),
            members: vec![racer_control_wire::Member {
                node: app.config.node.clone(),
                shares: std::num::NonZeroU32::new(1).unwrap(),
                peer_endpoint: "127.0.0.1:7443".into(),
                rails: vec![racer_control_wire::RailMapping {
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
        worker.snapshots.apply(publication.clone()).unwrap();
        app.node.inventory.update(vec![]).unwrap();
        worker.poll_native(&mut cx).unwrap();
        assert!(worker.native_publication().unwrap().is_empty());
        let fresh = crate::rdma::inventory();
        app.node.inventory.update(fresh.clone()).unwrap();
        for _ in 0..20 {
            worker.poll_native(&mut cx).unwrap();
            service
                .poll(&mut Context::from_waker(futures::task::noop_waker_ref()), 8)
                .unwrap();
        }
        assert_eq!(worker.actual_rails[0].device, "new");
        assert!(worker.links.as_ref().unwrap().rail_ready(RailId(7)));
        // No membership change is needed for local discovery withdrawal to revoke.
        app.node.inventory.update(vec![]).unwrap();
        worker.refresh_inventory().unwrap();
        assert!(!worker.links.as_ref().unwrap().rail_ready(RailId(7)));
        service
            .poll(
                &mut Context::from_waker(futures::task::noop_waker_ref()),
                256,
            )
            .unwrap();
        assert_eq!(worker.runtime.admission.used(ResourceClass::Registered), 0);
        app.node.inventory.update(fresh.clone()).unwrap();
        for _ in 0..20 {
            worker.poll_native(&mut cx).unwrap();
            service
                .poll(&mut Context::from_waker(futures::task::noop_waker_ref()), 8)
                .unwrap();
        }
        assert!(worker.links.as_ref().unwrap().rail_ready(RailId(7)));
        let mut changed = fresh;
        changed.iter_mut().find(|n| n.device == "new").unwrap().gid = Some([3; 16]);
        app.node.inventory.update(changed).unwrap();
        worker.refresh_inventory().unwrap();
        assert!(!worker.links.as_ref().unwrap().rail_ready(RailId(7)));
        service
            .poll(
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
        for (class, floor) in [
            (ResourceClass::Plaintext, 3 * page),
            (
                ResourceClass::Ciphertext,
                3 * (page + 16) + crate::store::MAX_HEADER_BYTES,
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
        too_small.plaintext_bytes = NonZeroUsize::new(3 * page - 1).unwrap();
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
        use racer_platform::group::affinity::EffectiveTopology;
        AffinityPlan::from_topology(
            config,
            EffectiveTopology {
                cpus: (0..cores)
                    .map(|cpu| crate::worker::cpu_location(cpu, Some(0), Some(cpu), None))
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
        use racer_platform::group::affinity::EffectiveTopology;
        let plan = AffinityPlan::from_topology(
            config,
            EffectiveTopology {
                cpus: (0..8)
                    .map(|cpu| crate::worker::cpu_location(cpu, Some(0), Some(cpu), None))
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
        use racer_platform::group::affinity::EffectiveTopology;
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
                            .map(|cpu| {
                                crate::worker::cpu_location(cpu, Some(0), Some(cpu % 4), Some(0))
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
            let floor = 3 * crate::model::PAGE_BYTES as usize;
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
        use racer_platform::group::affinity::EffectiveTopology;
        let mut config = default_config(false);
        config.scale_limits(11).unwrap();
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
                        .map(|cpu| {
                            crate::worker::cpu_location(
                                cpu,
                                Some(0),
                                Some(cpu),
                                Some(usize::from(cpu >= 7)),
                            )
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
    fn default_worker_sizing_funds_native_slots_within_node_budgets() {
        let mut config = default_config(true);
        assert_eq!(config.limits.registered_bytes.get(), 96 * 1024 * 1024);
        let mut plan = four_pair_plan(&config);
        config.scale_limits(plan.pairs.len()).unwrap();
        let limits = size_workers(&config.limits, &mut plan, true).unwrap();
        assert_eq!(plan.pairs.len(), 5);
        assert_eq!(plan.max_threads, usize::MAX);
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
            5 * 96 * 1024 * 1024 / crate::rdma::BUFFER_LEN
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
    fn native_worker_sizing_requires_two_node_wide_buffers() {
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
            if expected < 2 {
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
    fn native_preparation_funds_two_buffers_or_falls_back_to_http() {
        let mut limits = default_config(true).limits;
        let buffer = crate::rdma::BUFFER_LEN;
        for (budget, funded) in [(buffer, false), (2 * buffer - 1, false), (2 * buffer, true)] {
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
            2,
            None,
        );
        let admission = flow_control::Quotas::new(AdmissionPolicy::new(limits.clone()));
        let held = admission
            .reserve(None, ResourceClass::Registered, 1)
            .unwrap();
        let startup = scope(Duration::from_secs(10)).unwrap();
        let publication = vec![racer_control_wire::RailMapping {
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
        app.discovered_nics = vec![racer_control_wire::RailMapping {
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
        let (io, engine) = security::pair(WorkerId(0), 0, app.limits.queue_entries);
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
            .apply(Publication {
                schema_version: 1,
                cluster: app.config.cluster.clone(),
                sequence: PublicationSequence(1),
                membership_version: MembershipVersion(1),
                members: vec![racer_control_wire::Member {
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
        use rdma_verbs::simulation::Device;
        use rdma_verbs::simulation::Simulation;
        let sim = Simulation::new();
        let simulated = sim
            .with_devices(vec![Device::new("test-device", [1; 16])])
            .unwrap();
        let _environment = simulated.enter();
        let mut app = configured();
        app.discovered_nics[0].device = "test-device".into();
        let (mut worker, engine) = worker(&app, true, true);
        assert_eq!(worker.native_publication().unwrap().len(), 1);
        let mut service = app.build_crypto(WorkerId(0), engine).unwrap();
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        for _ in 0..20 {
            worker.poll_native(&mut cx).unwrap();
            service
                .poll(&mut Context::from_waker(futures::task::noop_waker_ref()), 8)
                .unwrap();
        }
        assert!(worker.links.as_ref().unwrap().rail_ready(RailId(7)));
        let charged = worker.runtime.admission.used(ResourceClass::Registered);
        assert!(charged > 0);
        worker.links.as_ref().unwrap().close();
        worker.actual_rails.clear();
        service
            .poll(
                &mut Context::from_waker(futures::task::noop_waker_ref()),
                256,
            )
            .unwrap();
        assert_eq!(worker.runtime.admission.used(ResourceClass::Registered), 0);
        worker.native_retry = racer_platform::environment::now();
        for _ in 0..20 {
            worker.poll_native(&mut cx).unwrap();
            service
                .poll(&mut Context::from_waker(futures::task::noop_waker_ref()), 8)
                .unwrap();
        }
        assert!(worker.links.as_ref().unwrap().rail_ready(RailId(7)));
        assert_eq!(
            worker.runtime.admission.used(ResourceClass::Registered),
            charged
        );
        worker
            .snapshots
            .apply(Publication {
                schema_version: 1,
                cluster: app.config.cluster.clone(),
                sequence: PublicationSequence(2),
                membership_version: MembershipVersion(2),
                members: vec![racer_control_wire::Member {
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
        assert!(!worker.links.as_ref().unwrap().rail_ready(RailId(7)));
        service
            .poll(
                &mut Context::from_waker(futures::task::noop_waker_ref()),
                256,
            )
            .unwrap();
        assert_eq!(worker.runtime.admission.used(ResourceClass::Registered), 0);
        drop(service);
        assert_eq!(sim.live_resources(), 0);
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
            assert!(!worker.links.as_ref().unwrap().rail_ready(RailId(7)));
            assert_eq!(worker.runtime.admission.used(ResourceClass::Registered), 0);
        }
    }

    #[test]
    fn local_configuration_cannot_replace_missing_local_membership() {
        let app = configured();
        let (mut worker, _engine) = worker(&app, true, true);
        worker
            .snapshots
            .apply(Publication {
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
        assert!(!worker.links.as_ref().unwrap().rail_ready(RailId(7)));
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
                    service
                        .poll(&mut Context::from_waker(futures::task::noop_waker_ref()), 1)
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
        activation_falls_back();
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
        activation_falls_back();
    }
}
