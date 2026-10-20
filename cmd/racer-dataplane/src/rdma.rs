use crate::admission::AdmissionPolicy;
use crate::admission::ResourceClass;
use crate::error::Error;
use crate::error::Operation;
use crate::error::Result;
use crate::memory::BufferPool;
use crate::memory::CiphertextPage;
use crate::model::PageEnvelope;
use crate::model::PageId;
use crate::model::TransferId;
use crate::peer::protocol::VerifiedHead;
use crate::runtime::RequestScope;
use crate::security::PageCryptoEngine;
use crate::topology::FAILURE_LINKS;
use crate::topology::Route;
use base64::Engine;
use base64::engine::general_purpose::STANDARD;
#[cfg(test)]
use racer_control_wire::MembershipVersion;
use racer_control_wire::NodeId;
use racer_control_wire::RailId;
use racer_control_wire::RailMapping;
use rdma_verbs::PortInfo;
use rdma_verbs::discovery::Binding;
use sha2::Digest;
use sha2::Sha256;
use std::cell::Cell;
use std::cell::RefCell;
use std::future::poll_fn;
use std::path::Path;
use std::rc::Rc;
use std::task::Poll;

pub(crate) mod link_setup;
pub(crate) mod links;
mod retry;
pub use links::AuthenticatedGrant;
pub use links::BUFFER_LEN;
pub use links::GRANT_HEADER;
pub use links::LinkNode;
pub use links::Links;
pub use links::NativeSent;
pub use links::PendingReceive;
pub use links::SETUP_DEADLINE;
pub use links::SetupAdmission;
pub use links::buffer_count;
pub use links::grants_per_link;
pub use links::link_key;
use retry::NativeRetry;

/// Host the node's link owner beside page crypto on its paired thread.
pub type WithOwner = rdma_verbs::WithNative<PageCryptoEngine, rdma_verbs::Owner>;

// Match discovered ports to trusted local fabric associations and publication.
// Fabric strings are opaque labels: a GID or enumeration order is never a label.
// Public contracts always compile; native FFI is gated by `rdma`. The node owner
// runs on one crypto thread (`WithOwner`); I/O workers use `Links`.

/// Signed setup extension of an `RdmaLink` request and response.
pub const SETUP_HEADER: &str = "racer-rdma-setup";
pub const SETUP_BINDING_HEADER: &str = "racer-rdma-setup-binding";
/// Signed `Complete` extension naming the written grant.
pub const COMPLETION_HEADER: &str = "racer-rdma-complete";
/// The security owner must cover these extension headers in the signature's
/// component list. VerifiedHead establishes identity and freshness; the connection
/// session performs replay admission before native control dispatch.
pub(crate) fn signed_value(head: &VerifiedHead, name: &str, bound: usize) -> Result<Vec<u8>> {
    let value = head.signed.head.unique(name)?.ok_or(Error::Unauthorized)?;
    if value.len() > bound * 2 {
        return Err(Error::InvalidRequest);
    }
    // Require the extension to be explicitly signed, not merely present in a
    // verified message. The signature-input grammar uses quoted component names.
    let input = head
        .signed
        .head
        .unique("signature-input")?
        .ok_or(Error::Unauthorized)?;
    let quoted = format!("\"{name}\"");
    if !input.windows(quoted.len()).any(|w| w == quoted.as_bytes()) {
        return Err(Error::Unauthorized);
    }
    let bytes = STANDARD.decode(value).map_err(|_| Error::InvalidRequest)?;
    if bytes.len() > bound || STANDARD.encode(&bytes).as_bytes() != value {
        return Err(Error::InvalidRequest);
    }
    Ok(bytes)
}
fn validate_envelope(envelope: &PageEnvelope) -> Result<()> {
    if envelope.plaintext_length == 0
        || envelope.plaintext_length > 16 * 1024 * 1024
        || envelope.ciphertext_length != envelope.plaintext_length + 16
    {
        return Err(Error::InvalidRange);
    }
    Ok(())
}

/// Largest ciphertext page: 16 MiB of plaintext plus the AEAD tag.
pub const MAX_CIPHERTEXT: usize = 16 * 1024 * 1024 + 16;
fn validate_native_page_size(page_size: libc::c_long) -> Result<()> {
    if page_size == 4096 {
        Ok(())
    } else {
        Err(Error::Unavailable)
    }
}
/// Match authenticated physical bindings against live eligible verbs ports.
pub fn match_publication(
    publication: &[RailMapping],
    discovered: &[PortInfo],
) -> Result<Vec<(RailMapping, usize)>> {
    Ok(rdma_verbs::discovery::match_ports(
        &publication.iter().map(binding).collect::<Vec<_>>(),
        discovered,
    )?
    .into_iter()
    .map(|(port, index)| (mapping(port), index))
    .collect())
}

// Pre-enrollment inventory and deterministic per-worker physical NIC selection.

/// Shared durable-journal bound for admission, restoration, and enrollment I/O.
pub use rdma_verbs::discovery::MAX_JOURNAL_BYTES;

/// One process-wide inventory, shared by enrollment and every worker. Withdrawn
/// ports keep their rail reservation: neither outages nor GID changes renumber
/// surviving ports. Enrollment persists reservations across process restarts.
#[derive(Default)]
pub struct Inventory(rdma_verbs::discovery::Inventory);

/// Local discovery snapshot translated into Racer publication types.
#[derive(Clone, Default)]
pub struct Snapshot {
    pub generation: u64,

    pub nics: Vec<RailMapping>,
}

impl Inventory {
    pub fn generation(&self) -> u64 {
        self.0.generation()
    }
    /// Read the shared physical inventory using Racer's publication types.
    pub fn snapshot(&self) -> Result<Snapshot> {
        Ok(snapshot(self.0.snapshot()?))
    }

    /// Optional transport discovery failures withdraw RDMA, not HTTP service.
    pub fn refresh(&self) -> Result<Snapshot> {
        self.update(inventory())
    }

    /// Publish discovered ports while preserving their durable physical labels.
    pub fn update(&self, nics: Vec<RailMapping>) -> Result<Snapshot> {
        Ok(snapshot(self.0.update(nics.iter().map(binding).collect())?))
    }

    /// Keep the existing enrollment journal representation unchanged.
    pub fn reservations(&self) -> Result<Vec<u8>> {
        self.0.reservations().map_err(Into::into)
    }

    /// Restore before issuance, never discard a corrupt journal and renumber.
    pub fn restore(&self, bytes: &[u8]) -> Result<()> {
        self.0.restore(bytes).map_err(|error| match error {
            rdma_verbs::Error::InvalidRequest => Error::CorruptRecord,
            other => other.into(),
        })
    }
}

/// Erase application rail identity before passing physical bindings to verbs.
fn binding(nic: &RailMapping) -> Binding {
    Binding {
        rail: nic.rail.0,
        device: nic.device.clone(),
        port: nic.port,
        gid: nic.gid,
        numa_node: nic.numa_node,
    }
}

/// Restore application identity after physical discovery, without authorizing it.
fn mapping(nic: Binding) -> RailMapping {
    RailMapping {
        rail: RailId(nic.rail),
        device: nic.device,
        port: nic.port,
        gid: nic.gid,
        numa_node: nic.numa_node,
    }
}

/// Convert the physical snapshot at the application boundary.
fn snapshot(snapshot: rdma_verbs::discovery::Snapshot) -> Snapshot {
    Snapshot {
        generation: snapshot.generation,
        nics: snapshot.nics.into_iter().map(mapping).collect(),
    }
}

/// Discovery failures are optional-transport failures, never HTTP startup failures.
pub fn inventory() -> Vec<RailMapping> {
    inventory_at(
        rdma_verbs::inventory().unwrap_or_default(),
        Path::new("/sys/class/infiniband"),
    )
}

fn inventory_at(ports: Vec<PortInfo>, root: &Path) -> Vec<RailMapping> {
    rdma_verbs::discovery::inventory_at(ports, root)
        .into_iter()
        .take(64)
        .enumerate()
        .map(|(i, p)| RailMapping {
            device: p.device,
            port: p.port,
            rail: RailId(i as u16),
            gid: Some(p.gid),
            numa_node: p.numa_node,
        })
        .collect()
}

/// Select at most one device per rail. Explicit NUMA overrides detected locality;
/// prefer local, then unknown, then remote, spreading equal candidates by worker.
pub fn select_worker(
    published: &[RailMapping],
    discovered: &[RailMapping],
    worker: usize,
    numa: Option<usize>,
    capacity: usize,
) -> Vec<RailMapping> {
    rdma_verbs::discovery::select_worker(
        &published.iter().map(binding).collect::<Vec<_>>(),
        &discovered.iter().map(binding).collect::<Vec<_>>(),
        worker,
        numa,
        capacity,
    )
    .into_iter()
    .map(mapping)
    .collect()
}

// RDMA requires compatible authenticated mappings; discovery can only veto.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum TransportPlan {
    Http,
    Rdma { rail: RailId },
}
/// Conservative route summary; actual transport admission uses select_hop.
pub fn select(route: &Route, page: &PageId) -> Result<TransportPlan> {
    validate_route(route)?;
    let members = route
        .nodes
        .iter()
        .map(|node| route.membership.member(node))
        .collect::<Result<Vec<_>>>()?;
    select_members(&route.membership, &members, page)
}
pub fn select_hop(
    route: &Route,
    page: &PageId,
    local: &racer_control_wire::NodeId,
    peer: &racer_control_wire::NodeId,
) -> Result<TransportPlan> {
    validate_route(route)?;
    if !route.nodes.windows(2).any(|pair| {
        (&pair[0] == local && &pair[1] == peer) || (&pair[1] == local && &pair[0] == peer)
    }) {
        return Err(Error::IncompatibleMembership);
    }
    select_members(
        &route.membership,
        &[
            route.membership.member(local)?,
            route.membership.member(peer)?,
        ],
        page,
    )
}
fn validate_route(route: &Route) -> Result<()> {
    if route.nodes.is_empty()
        || route.nodes.len() > usize::from(FAILURE_LINKS) + 1
        || route
            .nodes
            .iter()
            .enumerate()
            .any(|(i, node)| route.nodes[..i].contains(node))
    {
        return Err(Error::InvalidRequest);
    }
    for node in &route.nodes {
        route.membership.member(node)?;
    }
    Ok(())
}
fn select_members(
    membership: &crate::topology::Membership,
    members: &[&crate::topology::Member],
    page: &PageId,
) -> Result<TransportPlan> {
    if members
        .iter()
        .any(|m| m.site.is_empty() || m.site != members[0].site || m.rails.is_empty())
    {
        return Ok(TransportPlan::Http);
    }
    // Site is an admission boundary, not a new rail domain or hash scheme.
    let domain = membership.rail_domain();
    if domain.is_empty() {
        return Ok(TransportPlan::Http);
    }
    let mut digest = crate::topology::hash_domain(b"racer/rail/v1\0");
    crate::topology::hash_object(&mut digest, &page.version.object, page.number);
    let digest = crate::topology::hash_finish(digest);
    let sample = u64::from_be_bytes(digest[..8].try_into().unwrap());
    let rail = domain[(sample % domain.len() as u64) as usize];
    if !members
        .iter()
        .all(|m| m.rails.iter().any(|m| m.rail == rail))
    {
        return Ok(TransportPlan::Http);
    }
    Ok(TransportPlan::Rdma { rail })
}

#[cfg(test)]
pub(crate) mod tests {
    use super::*;

    pub(crate) mod session_tests {
        use super::*;
        #[test]
        fn real_signed_setup_rejects_tampering_and_replay() {
            use crate::peer::protocol::Signatures;
            use crate::peer::protocol::SignedHead;
            use crate::test_support::security::CLUSTER;
            use crate::test_support::security::NODE;
            use crate::test_support::security::issued;
            use http1::Header;
            use http1::MessageHead;
            use http1::StartLine;
            use racer_control_wire::BundleGeneration;
            use racer_control_wire::ClusterId;
            use racer_control_wire::KeyringBundle;
            use racer_control_wire::SCHEMA_VERSION;
            use racer_crypto::identity::Certificates;
            use racer_crypto::identity::KeyEpochs;
            use racer_crypto::identity::Keyring;
            use std::sync::Arc;
            let (pending, chain, roots) = issued();
            let cluster = ClusterId(CLUSTER.into());
            let node = NodeId(NODE.into());
            let identity = pending
                .accept(cluster.clone(), node.clone(), chain, &roots)
                .unwrap();
            let keys = Rc::new(Keyring::new(
                cluster.clone(),
                node.clone(),
                Arc::new(KeyEpochs::default()),
            ));
            keys.install(KeyringBundle {
                schema_version: SCHEMA_VERSION,
                cluster: cluster.clone(),
                generation: BundleGeneration(1),
                peer_trust_roots: roots,
                cache_keys: vec![],
            })
            .unwrap();
            keys.install_signing_identity(Arc::new(identity)).unwrap();
            let certificates = Rc::new(Certificates::new(cluster, keys.clone()));
            let signatures = Signatures::new(keys, certificates);
            let setup = link_setup::Setup::new(RailId(9), [1; 32], 8, 2)
                .unwrap()
                .encode();
            let value = STANDARD.encode(&setup).into_bytes();
            let binding_value = STANDARD.encode(link_setup::binding(&setup)).into_bytes();
            let head = || MessageHead {
                start: StartLine::Request {
                    method: "POST".into(),
                    target: "/racer/peer/v1/rdma".into(),
                },
                headers: vec![
                    Header {
                        name: "racer-receiver".into(),
                        value: NODE.as_bytes().to_vec(),
                    },
                    Header {
                        name: SETUP_HEADER.into(),
                        value: value.clone(),
                    },
                    Header {
                        name: SETUP_BINDING_HEADER.into(),
                        value: binding_value.clone(),
                    },
                ],
            };
            let verified = signatures
                .verify_proof(signatures.sign(head()).unwrap())
                .unwrap();
            let signed = signed_value(&verified, SETUP_HEADER, 128).unwrap();
            assert_eq!(signed, setup);
            assert_eq!(link_setup::rail_of(&signed).unwrap(), RailId(9));
            assert!(link_setup::Setup::decode(&signed, RailId(8)).is_err());
            assert_eq!(
                signed_value(&verified, SETUP_BINDING_HEADER, 32).unwrap(),
                Sha256::digest(&setup).as_slice()
            );
            // A value present in the verified head but outside the signature's
            // component list is refused.
            assert_eq!(
                signed_value(&verified, "racer-unsigned", 32),
                Err(Error::Unauthorized)
            );
            assert_eq!(
                signed_value(&verified, SETUP_HEADER, 8),
                Err(Error::InvalidRequest)
            );
            let replay = SignedHead {
                head: verified.signed.head,
                signature: verified.signed.signature,
            };
            signatures.verify_proof(replay).unwrap();
            let mut tampered = signatures.sign(head()).unwrap();
            tampered
                .head
                .headers
                .iter_mut()
                .find(|h| h.name == SETUP_HEADER)
                .unwrap()
                .value[0] = b'A';
            assert!(signatures.verify_proof(tampered).is_err());
        }
    }

    pub(crate) mod registered_tests {
        use super::*;
        #[test]
        fn native_page_policy_preserves_unavailable_for_unsupported_hosts() {
            assert_eq!(validate_native_page_size(4096), Ok(()));
            for page_size in [-1, 0, 1024, 8192, 65536] {
                assert_eq!(
                    validate_native_page_size(page_size),
                    Err(Error::Unavailable)
                );
            }
        }
    }

    pub(crate) mod publication_tests {
        use super::*;
        #[test]
        fn discovery_matches_physical_ports_and_accepts_numa_override() {
            let publication = vec![RailMapping {
                rail: RailId(7),
                device: "mlx5_0".into(),
                port: 1,
                gid: Some([1; 16]),
                numa_node: Some(1),
            }];
            let port = PortInfo {
                device: "mlx5_0".into(),
                port: 1,
                gid: [1; 16],
                numa_node: Some(1),
            };
            assert!(match_publication(&publication, &[]).is_err());
            assert!(
                match_publication(
                    &[publication[0].clone(), publication[0].clone()],
                    std::slice::from_ref(&port)
                )
                .is_err()
            );
            assert!(match_publication(&publication, &[port.clone(), port.clone()]).is_err());
            assert!(
                match_publication(
                    &publication,
                    &[PortInfo {
                        numa_node: Some(0),
                        ..port.clone()
                    }]
                )
                .is_ok()
            );
            assert_eq!(
                match_publication(&publication, &[port]).unwrap()[0].0,
                publication[0]
            );
        }
    }

    pub(crate) mod discovery_tests {
        use super::*;
        #[test]
        fn journal_byte_admission_preserves_known_ports_and_restart_at_saturation() {
            // Exercise both maximal ordinary names and JSON-escaped local names.
            for fill in ['x', '"'] {
                let inventory = Inventory::default();
                let nic = |i| RailMapping {
                    device: format!("{i:04}{}", fill.to_string().repeat(59)),
                    port: 255,
                    rail: RailId(0),
                    gid: Some([1; 16]),
                    numa_node: None,
                };
                let mut accepted = 0;
                for batch in 0..20 {
                    let snapshot = inventory
                        .update((batch * 64..(batch + 1) * 64).map(nic).collect())
                        .unwrap();
                    accepted += snapshot.nics.len();
                    assert!(inventory.reservations().unwrap().len() <= MAX_JOURNAL_BYTES);
                }
                assert!(
                    accepted > 0 && accepted < 1024,
                    "byte bound must precede identity cap"
                );
                let journal = inventory.reservations().unwrap();
                assert!(inventory.update(vec![nic(2000)]).unwrap().nics.is_empty());
                assert_eq!(inventory.reservations().unwrap(), journal);
                let restarted = Inventory::default();
                restarted.restore(&journal).unwrap();
                let mut old = nic(0);
                old.gid = Some([2; 16]);
                let snapshot = restarted.update(vec![nic(2000), old]).unwrap();
                assert_eq!(snapshot.nics.len(), 1);
                assert_eq!(snapshot.nics[0].rail, RailId(0));
                assert_eq!(snapshot.nics[0].gid, Some([2; 16]));
                assert_eq!(restarted.reservations().unwrap(), journal);
            }
        }
        #[test]
        fn journal_restore_validates_names_ports_and_encoded_bounds_atomically() {
            let inventory = Inventory::default();
            inventory.restore(br#"[["valid",1,0]]"#).unwrap();
            let original = inventory.reservations().unwrap();
            for device in [
                "".to_owned(),
                ".".into(),
                "..".into(),
                "a/b".into(),
                "a\0b".into(),
                "a\nb".into(),
                "a\rb".into(),
                "x".repeat(64),
            ] {
                let bytes = serde_json::to_vec(&vec![(device.clone(), 1u8, 0u16)]).unwrap();
                assert_eq!(inventory.restore(&bytes), Err(Error::CorruptRecord));
                assert!(
                    inventory
                        .update(vec![RailMapping {
                            device,
                            port: 1,
                            rail: RailId(0),
                            gid: None,
                            numa_node: None
                        }])
                        .unwrap()
                        .nics
                        .is_empty()
                );
                assert_eq!(inventory.reservations().unwrap(), original);
            }
            assert_eq!(
                inventory.restore(br#"[["valid",0,0]]"#),
                Err(Error::CorruptRecord)
            );
            let oversized = serde_json::to_vec(
                &(0..1024)
                    .map(|i| (format!("{i:04}{}", "x".repeat(59)), 255u8, i as u16))
                    .collect::<Vec<_>>(),
            )
            .unwrap();
            assert!(oversized.len() > MAX_JOURNAL_BYTES);
            assert_eq!(inventory.restore(&oversized), Err(Error::CorruptRecord));
            let mut exact = original.clone();
            exact.resize(MAX_JOURNAL_BYTES, b' ');
            inventory.restore(&exact).unwrap();
            exact.push(b' ');
            assert_eq!(inventory.restore(&exact), Err(Error::CorruptRecord));
            assert_eq!(inventory.reservations().unwrap(), original);
        }
        #[test]
        fn reservations_survive_withdrawal_gid_change_hotplug_and_restart() {
            let nic = |device: &str, gid| RailMapping {
                device: device.into(),
                port: 1,
                rail: RailId(0),
                gid: Some([gid; 16]),
                numa_node: None,
            };
            let inventory = Inventory::default();
            assert!(inventory.update(vec![]).unwrap().nics.is_empty());
            let first = inventory.update(vec![nic("a", 1), nic("b", 2)]).unwrap();
            assert_eq!(first.nics[1].rail, RailId(1));
            let withdrawn = inventory.update(vec![nic("b", 3)]).unwrap();
            assert_eq!(withdrawn.nics[0].rail, RailId(1));
            assert!(withdrawn.generation > first.generation);
            let journal = inventory.reservations().unwrap();
            let restarted = Inventory::default();
            restarted.restore(&journal).unwrap();
            let next = restarted.update(vec![nic("b", 3), nic("c", 4)]).unwrap();
            assert_eq!(
                next.nics.iter().map(|n| n.rail).collect::<Vec<_>>(),
                vec![RailId(1), RailId(2)]
            );
            assert_eq!(
                restarted.update(vec![nic("a", 5)]).unwrap().nics[0].rail,
                RailId(0)
            );
            assert!(
                restarted
                    .update(vec![nic("a", 5), nic("a", 5)])
                    .unwrap()
                    .nics
                    .is_empty()
            );
            assert!(restarted.restore(b"bad").is_err());
            assert!(restarted.restore(br#"[["a",1,0],["b",1,0]]"#).is_err());
        }
        #[test]
        fn sysfs_pci_order_precedes_device_names_and_ports_have_ordinal_rails() {
            use std::os::unix::fs::symlink;
            struct Scratch(std::path::PathBuf);
            impl Drop for Scratch {
                fn drop(&mut self) {
                    std::fs::remove_dir_all(&self.0).unwrap();
                }
            }
            let dir = Scratch(
                Path::new(env!("CARGO_MANIFEST_DIR"))
                    .join("target")
                    .join(format!("nic-sysfs-{}", std::process::id())),
            );
            std::fs::create_dir_all(&dir.0).unwrap();
            for (name, bdf, numa) in [
                ("z", "0000:01:00.0", "2\n"),
                ("a", "0000:02:00.0", "-1\n"),
                ("b", "0000:03:00.0", "bad"),
            ] {
                std::fs::create_dir_all(dir.0.join(bdf)).unwrap();
                std::fs::create_dir_all(dir.0.join(name)).unwrap();
                std::fs::write(dir.0.join(bdf).join("numa_node"), numa).unwrap();
                symlink(dir.0.join(bdf), dir.0.join(name).join("device")).unwrap();
            }
            let port = |name: &str, port| PortInfo {
                device: name.into(),
                port,
                gid: [1; 16],
                numa_node: None,
            };
            let nics = inventory_at(
                vec![port("a", 1), port("z", 2), port("b", 1), port("z", 1)],
                &dir.0,
            );
            assert_eq!(
                nics.iter()
                    .map(|n| (n.device.as_str(), n.port, n.rail.0, n.numa_node))
                    .collect::<Vec<_>>(),
                vec![
                    ("z", 1, 0, Some(2)),
                    ("z", 2, 1, Some(2)),
                    ("a", 1, 2, None),
                    ("b", 1, 3, None)
                ]
            );
        }
        #[test]
        fn inventory_absence_and_unknown_numa_are_safe() {
            let root = Path::new("/nonexistent-racer-infiniband");
            assert!(inventory_at(vec![], root).is_empty());
            let port = |device: &str, port| PortInfo {
                device: device.into(),
                port,
                gid: [1; 16],
                numa_node: Some(99),
            };
            let nics = inventory_at(vec![port("z", 2), port("a", 1), port("../bad", 1)], root);
            assert_eq!(nics.len(), 2);
            assert_eq!(nics[0].device, "a");
            assert_eq!(nics[1].rail, RailId(1));
            assert!(nics.iter().all(|n| n.numa_node.is_none()));
        }
        #[test]
        fn worker_local_unknown_remote_override_and_same_rail_spreading() {
            let nic = |device: &str, numa| RailMapping {
                device: device.into(),
                port: 1,
                rail: RailId(7),
                gid: Some([1; 16]),
                numa_node: numa,
            };
            let detected = vec![
                nic("a", Some(0)),
                nic("b", Some(0)),
                nic("c", None),
                nic("d", Some(2)),
            ];
            let selected = |worker, numa| select_worker(&detected, &detected, worker, numa, 64);
            assert_eq!(selected(0, Some(0))[0].device, "a");
            assert_eq!(selected(1, Some(0))[0].device, "b");
            assert_eq!(selected(0, Some(1))[0].device, "c");
            assert_eq!(selected(0, Some(2))[0].device, "d");
            let override_nic = nic("a", Some(3));
            assert_eq!(
                select_worker(&[override_nic], &detected, 0, Some(3), 1)[0].numa_node,
                Some(3)
            );
            assert_eq!(
                select_worker(&detected[..2], &detected, 0, Some(9), 1).len(),
                1
            );
            assert!(select_worker(&detected, &[], 0, None, 64).is_empty());
            assert!(select_worker(&[], &detected, 0, None, 64).is_empty());
            assert!(select_worker(&detected, &detected, 0, None, 0).is_empty());
        }
    }

    pub(crate) mod rails_tests {
        use super::*;
        use crate::model::*;
        use crate::topology::Membership;
        use crate::topology::tests::fixtures::member;
        use crate::topology::tests::fixtures::object;
        use std::sync::Arc;
        fn mappings() -> Vec<RailMapping> {
            vec![
                RailMapping {
                    rail: RailId(7),
                    device: "a".into(),
                    port: 1,
                    gid: None,
                    numa_node: Some(0),
                },
                RailMapping {
                    rail: RailId(2),
                    device: "b".into(),
                    port: 1,
                    gid: None,
                    numa_node: Some(1),
                },
            ]
        }
        fn page(number: u64) -> PageId {
            PageId {
                version: ObjectVersion {
                    object: object(),
                    etag: StrongEtag::test_value("\"v1\""),
                },
                number: PageNumber(number),
            }
        }
        // Fixture-side hardware veto for summary tests. Real sessions additionally
        // require device activation; this helper never grants transport admission.
        fn local_compatible(
            route: &Route,
            plan: &TransportPlan,
            local: &NodeId,
            discovered: &[RailMapping],
        ) -> Result<bool> {
            if !route.nodes.contains(local) {
                return Err(Error::IncompatibleMembership);
            }
            let member = route.membership.member(local)?;
            let TransportPlan::Rdma { rail } = plan else {
                return Ok(true);
            };
            let Some(published) = member.rails.iter().find(|m| m.rail == *rail) else {
                return Ok(false);
            };
            let mut matching = discovered.iter().filter(|m| m.rail == *rail);
            Ok(matching.next().is_some_and(|hardware| {
                hardware.device == published.device
                    && hardware.port == published.port
                    && published
                        .numa_node
                        .is_none_or(|numa| hardware.numa_node == Some(numa))
            }) && matching.next().is_none())
        }
        fn select_with_local(
            route: &Route,
            page: &PageId,
            local: &NodeId,
            discovered: &[RailMapping],
        ) -> Result<TransportPlan> {
            let plan = select(route, page)?;
            Ok(if local_compatible(route, &plan, local, discovered)? {
                plan
            } else {
                TransportPlan::Http
            })
        }
        fn route(change: impl FnOnce(&mut Vec<crate::topology::Member>)) -> Route {
            let mut members: Vec<_> = (0..3)
                .map(|i| {
                    let mut member = member(i, 4);
                    member.site = "site1".into();
                    member.rails = mappings();
                    member
                })
                .collect();
            change(&mut members);
            let membership = Arc::new(Membership::validate(MembershipVersion(1), members).unwrap());
            Route {
                nodes: membership
                    .members()
                    .iter()
                    .map(|m| m.node.clone())
                    .collect(),
                membership,
            }
        }
        #[test]
        fn mixed_site_hops_preserve_global_rail_mapping_and_hardware_vetoes() {
            let mixed = route(|m| m[2].site = "site2".into());
            let a = &mixed.nodes[0];
            let b = &mixed.nodes[1];
            let c = &mixed.nodes[2];
            assert_eq!(select(&mixed, &page(0)).unwrap(), TransportPlan::Http);
            for (from, to, expected) in [
                (a, b, TransportPlan::Rdma { rail: RailId(7) }),
                (b, a, TransportPlan::Rdma { rail: RailId(7) }),
                (b, c, TransportPlan::Http),
                (c, b, TransportPlan::Http),
            ] {
                assert_eq!(select_hop(&mixed, &page(0), from, to).unwrap(), expected);
            }
            for other in [c, a, &NodeId("unknown".into())] {
                assert!(select_hop(&mixed, &page(0), a, other).is_err());
            }
            for local in [0, 1] {
                let missing = route(|m| m[local].site.clear());
                assert_eq!(
                    select_hop(&missing, &page(0), a, b).unwrap(),
                    TransportPlan::Http
                );
            }
            for changed in [
                route(|m| m[1].site.clear()),
                route(|m| m[1].rails.clear()),
                route(|m| {
                    m[1].rails
                        .iter_mut()
                        .for_each(|r| r.rail = RailId(r.rail.0 + 1))
                }),
            ] {
                assert_eq!(
                    select_hop(&changed, &page(0), a, b).unwrap(),
                    TransportPlan::Http
                );
            }
            let plan = select_hop(&mixed, &page(0), a, b).unwrap();
            assert!(local_compatible(&mixed, &plan, a, &mappings()).unwrap());
            assert!(!local_compatible(&mixed, &plan, a, &[]).unwrap());
        }
        #[test]
        fn golden_page_to_rail_vectors() {
            let route = route(|_| {});
            for (number, rail) in [(0, 7), (1, 2), (u64::MAX, 2)] {
                assert_eq!(
                    select(&route, &page(number)).unwrap(),
                    TransportPlan::Rdma { rail: RailId(rail) }
                );
            }
        }
        #[test]
        fn repeated_rails_and_different_physical_names_do_not_change_remote_eligibility() {
            let original = route(|_| {});
            let repeated = route(|members| {
                for (i, member) in members.iter_mut().enumerate() {
                    let mut extra = member.rails[0].clone();
                    extra.device = format!("extra-{i}");
                    member.rails.push(extra);
                    member.rails[0].device = format!("local-{i}");
                }
            });
            assert_eq!(
                original.membership.rail_domain(),
                repeated.membership.rail_domain()
            );
            for number in 0..100 {
                assert_eq!(
                    select(&original, &page(number)),
                    select(&repeated, &page(number))
                );
            }
        }
        #[test]
        fn intersection_over_all_hops_and_http_fallback() {
            for route in [
                route(|m| m[1].site.clear()),
                route(|m| m[1].rails.clear()),
                route(|m| {
                    for rail in &mut m[1].rails {
                        rail.rail = RailId(rail.rail.0 + 1);
                    }
                }),
            ] {
                assert_eq!(select(&route, &page(0)).unwrap(), TransportPlan::Http);
            }
            let full = route(|_| {});
            let partial = route(|m| m[1].rails.retain(|rail| rail.rail == RailId(7)));
            for number in 0..100 {
                let expected = match select(&full, &page(number)).unwrap() {
                    TransportPlan::Rdma { rail: RailId(7) } => {
                        TransportPlan::Rdma { rail: RailId(7) }
                    }
                    _ => TransportPlan::Http,
                };
                assert_eq!(select(&partial, &page(number)).unwrap(), expected);
                let mut alternate = partial.clone();
                alternate.nodes.remove(1);
                assert_eq!(
                    select(&alternate, &page(number)).unwrap(),
                    select(&full, &page(number)).unwrap()
                );
                let mut version = page(number);
                version.version.etag = StrongEtag::test_value("\"v2\"");
                assert_eq!(
                    select(&full, &version).unwrap(),
                    select(&full, &page(number)).unwrap()
                );
            }
        }
        #[test]
        fn deterministic_reverse_path_order_and_local_hardware() {
            let route = route(|m| {
                m[1].rails.reverse();
                for rail in &mut m[1].rails {
                    rail.numa_node = Some(99);
                }
            });
            let mut reverse = route.clone();
            reverse.nodes.reverse();
            let mut selected = std::collections::BTreeSet::new();
            for number in 0..100 {
                let plan = select(&route, &page(number)).unwrap();
                assert_eq!(plan, select(&reverse, &page(number)).unwrap());
                let TransportPlan::Rdma { rail } = plan else {
                    panic!("expected RDMA");
                };
                selected.insert(rail);
                for (local, hardware, expected) in [
                    (0, mappings(), plan),
                    (1, mappings(), TransportPlan::Http),
                    (0, vec![], TransportPlan::Http),
                ] {
                    assert_eq!(
                        select_with_local(&route, &page(number), &route.nodes[local], &hardware)
                            .unwrap(),
                        expected
                    );
                }
            }
            assert_eq!(selected.len(), 2);
            let mut invalid = route.clone();
            invalid.nodes.push(invalid.nodes[0].clone());
            assert_eq!(select(&invalid, &page(0)), Err(Error::InvalidRequest));
        }
    }
}
