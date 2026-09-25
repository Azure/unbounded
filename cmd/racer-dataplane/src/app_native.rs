//! Native lifecycle endpoints share the existing I/O/crypto worker pair.
use super::*;
use crate::rdma::{
    device::FabricPort,
    lifecycle::{IoPort, NativePort, WithNative},
};
use std::collections::HashMap;

#[derive(Default)]
pub(super) struct NativePairs {
    ports: Mutex<HashMap<WorkerId, (Option<IoPort>, Option<NativePort>)>>,
}
impl NativePairs {
    pub(super) fn prepare(
        &self,
        workers: impl Iterator<Item = WorkerId>,
        limits: &Limits,
    ) -> Result<()> {
        let bytes = crate::rdma::registered::MAX_CIPHERTEXT;
        let charge = ((bytes + 4095) & !4095) * 2;
        let slots = (limits.registered_bytes.get() / charge)
            .min(limits.queue_entries.get())
            .min(256);
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
