//! Native lifecycle endpoints share the existing I/O/crypto worker pair.
use super::*;
use crate::rdma::{
    device::FabricPort,
    lifecycle::{IoPort, NativePort, WithNative},
};
use crate::runtime::collections::HashMap;

#[derive(Default)]
pub(super) struct NativePairs {
    ports: Mutex<HashMap<WorkerId, NativePair>>,
}
type NativePair = (Option<IoPort>, Option<NativePort>);

pub(super) fn slot_count(limits: &Limits) -> Result<usize> {
    let charge =
        crate::rdma::registered::native_slot_charge(crate::rdma::registered::MAX_CIPHERTEXT)?;
    Ok((limits.registered_bytes.get() / charge)
        .min(limits.queue_entries.get())
        .min(256))
}

impl NativePairs {
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
            let (io, native) = crate::rdma::lifecycle::pair(slots)?;
            ports.insert(worker, (Some(io), Some(native)));
        }
        Ok(())
    }
    pub(super) fn io(&self, worker: WorkerId) -> Result<Option<IoPort>> {
        Ok(self
            .ports
            .lock()
            .map_err(|_| Error::Unavailable)?
            .get_mut(&worker)
            .and_then(|pair| pair.0.take()))
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
            .and_then(|pair| pair.1.take());
        Ok(match native {
            Some(native) => Box::new(WithNative::new(engine, native)),
            None => Box::new(engine),
        })
    }
}
impl Application {
    /// Trusted startup associations, not a report of activated hardware or rails.
    pub fn fabric_ports(&self) -> &[FabricPort] {
        &self.fabric_ports
    }

    /// Trusted physical associations only. Rail IDs and alignment remain exclusively
    /// controller-owned. No I/O, native discovery, or extra threads are created.
    pub fn with_fabric_ports(mut self, ports: Vec<FabricPort>) -> Result<Self> {
        crate::config::validate_fabric_ports(&ports)?;
        self.fabric_ports = ports;
        Ok(self)
    }
}
impl WorkerApplication {
    pub(super) async fn activate_native(&mut self, startup: &RequestScope) -> Result<()> {
        let Some(devices) = &self.devices else {
            return Ok(());
        };
        let snapshot = self.snapshots.current()?;
        let member = snapshot.membership.member(self.keys.node())?;
        if !member.alignment_enabled || member.rails.is_empty() || self.fabric_ports.is_empty() {
            return Ok(());
        }
        match devices
            .activate(
                member.rails.clone(),
                self.fabric_ports.clone(),
                &self.runtime.admission,
                crate::rdma::registered::MAX_CIPHERTEXT,
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
        control::wire::{Publication, PublicationSequence},
        model::{identity::MembershipVersion, limits::ResourceClass},
        runtime::crypto::{self, CryptoClient},
        topology::{membership::Member, rails::RailId},
    };

    fn default_config(rdma: bool) -> Config {
        Config::from_lookup_with_fabric_ports(|name| {
            Ok(match name {
                "RACER_CLUSTER_ID" => Some("00000000-0000-4000-8000-000000000001".into()),
                "RACER_CONTROL_ENDPOINT" => Some("https://control.example".into()),
                "RACER_ENABLE_RDMA" => Some(rdma.to_string()),
                _ => None,
            })
        })
        .unwrap()
        .0
    }

    #[test]
    fn operator_profile_reaches_runtime_admission_and_progress_floors() {
        // Parse the actual deployed ConfigMap defaults, not a second test profile.
        let manifest = include_str!("../../../deploy/racer/dataplane-config.yaml.tmpl");
        let values: std::collections::HashMap<_, _> = manifest
            .lines()
            .filter_map(|line| line.trim().split_once(": \""))
            .filter(|(key, _)| key.starts_with("RACER_"))
            .map(|(key, value)| (key, value.trim_end_matches('"')))
            .collect();
        assert_eq!(values.len(), 10);
        let (config, _) = Config::from_lookup_with_fabric_ports(|name| {
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
        assert!(!config.enable_rdma);
        assert_eq!(config.max_threads, 8);
        assert_eq!(config.slab_bytes, 1024 * 1024 * 1024);
        let mut plan = four_pair_plan(&config);
        let limits = size_workers(&config.limits, &mut plan, config.enable_rdma).unwrap();
        assert_eq!(plan.pairs.len(), 4);
        let admission = Admission::new(limits.clone());
        let page = crate::model::range::PAGE_BYTES as usize;
        for (class, floor) in [
            (ResourceClass::Plaintext, 3 * page),
            (
                ResourceClass::Ciphertext,
                3 * (page + 16) + crate::store::format::MAX_HEADER_BYTES,
            ),
            (ResourceClass::DirtyCiphertext, page + 16),
            (ResourceClass::RequestContext, 128 * 1024),
        ] {
            let reservation = admission.reserve(None, class, floor).unwrap();
            assert_eq!(admission.used(class), floor);
            drop(reservation);
            assert_eq!(admission.used(class), 0);
        }
        assert_eq!(limits.plaintext_bytes.get(), 64 * 1024 * 1024);
        assert_eq!(limits.ciphertext_bytes.get(), 64 * 1024 * 1024);
        assert_eq!(limits.dirty_bytes.get(), 32 * 1024 * 1024);
        assert_eq!(limits.request_context_bytes.get(), 4 * 1024 * 1024);
        assert_eq!(admission.used(ResourceClass::Registered), 0);
        // Reducing thread cap or quotas keeps whole pairs and re-partitions the
        // node budget. An impossible byte floor must fail rather than deadlock.
        let mut too_small = config.limits.clone();
        too_small.plaintext_bytes = NonZeroUsize::new(3 * page - 1).unwrap();
        assert!(size_workers(&too_small, &mut plan, false).is_err());
    }

    fn four_pair_plan(config: &Config) -> AffinityPlan {
        use crate::runtime::affinity::{CpuLocation, EffectiveTopology};
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
        assert_eq!(plan.pairs.len(), 4);
        plan
    }

    #[test]
    fn default_worker_sizing_funds_native_slots_within_node_budgets() {
        let config = default_config(true);
        assert_eq!(config.limits.registered_bytes.get(), 128 * 1024 * 1024);
        let mut plan = four_pair_plan(&config);
        let limits = size_workers(&config.limits, &mut plan, true).unwrap();
        assert_eq!(plan.pairs.len(), 3);
        assert_eq!(plan.max_threads, 8);
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
            let devices = Devices::new(Rc::new(Verbs));
            devices.attach(port).unwrap();
            let admission = Admission::new(limits.clone());
            let startup = scope(Duration::from_secs(10)).unwrap();
            let mut activation = devices.activate(
                vec![],
                vec![],
                &admission,
                crate::rdma::registered::MAX_CIPHERTEXT,
                &startup,
            );
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
            assert_eq!(plan.pairs.len(), 4);
            assert_eq!(limits.registered_bytes, config.limits.registered_bytes);
            assert_eq!(
                limits.plaintext_bytes.get(),
                config.limits.plaintext_bytes.get() / 4
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
        let devices = Devices::new(Rc::new(Verbs));
        devices.attach(port).unwrap();
        let admission = Admission::new(limits);
        let held = admission
            .reserve(None, ResourceClass::Registered, 1)
            .unwrap();
        let startup = scope(Duration::from_secs(10)).unwrap();
        assert!(matches!(
            futures::executor::block_on(devices.activate(
                vec![],
                vec![],
                &admission,
                crate::rdma::registered::MAX_CIPHERTEXT,
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
        let (mut config, ports) = Config::from_lookup_with_fabric_ports(|name| {
            Ok(match name {
                "RACER_CLUSTER_ID" => Some("00000000-0000-4000-8000-000000000001".into()),
                "RACER_CONTROL_ENDPOINT" => Some("https://control.example".into()),
                "RACER_ENABLE_RDMA" => Some("true".into()),
                "RACER_MAX_THREADS" => Some("2".into()),
                "RACER_FABRIC_PORTS" => {
                    Some(r#"[{"fabric":"trusted","device":"missing","port":1}]"#.into())
                }
                _ => None,
            })
        })
        .unwrap();
        // Stand in only for the identity returned by authenticated enrollment.
        config.node = NodeId("00000000-0000-4000-8000-000000000002".into());
        Application::assemble(config)
            .unwrap()
            .with_fabric_ports(ports)
            .unwrap()
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
        let admission = Rc::new(Admission::new(app.limits.clone()));
        let (io, engine) = crypto::pair(WorkerId(0), 0, app.limits.queue_entries);
        let runtime = WorkerRuntime {
            reactor: Rc::new(Reactor::new(admission.clone())),
            admission,
            crypto: Rc::new(CryptoClient::new(io)),
        };
        let mut worker =
            WorkerApplication::assemble(&app.config, &app.node, WorkerId(0), runtime).unwrap();
        worker.fabric_ports = app.fabric_ports.clone();
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
                    rails: if published_rails {
                        vec![crate::topology::rails::RailMapping {
                            rail: RailId(7),
                            fabric: "trusted".into(),
                            numa_node: None,
                        }]
                    } else {
                        vec![]
                    },
                    alignment_enabled: aligned,
                }],
                caches: vec![],
            })
            .unwrap();
        (worker, CryptoRuntime { port: engine })
    }

    #[test]
    fn programmatic_associations_share_config_validation() {
        let mut ports = configured().fabric_ports.clone();
        ports.push(ports[0].clone());
        assert!(configured().with_fabric_ports(ports).is_err());
        let mut ports = configured().fabric_ports.clone();
        ports[0].device = "../device".into();
        assert!(configured().with_fabric_ports(ports).is_err());
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
                app.fabric_ports.clear();
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
        assert_eq!(app.fabric_ports[0].fabric, "trusted");
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
