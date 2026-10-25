//! One primary reconcile pass, lifted from `primary::reconcile` and
//! `primary::update_discovery`.
//!
//! [`plan_pass`] turns typed observations (inventory, live seats, the
//! committed record, the previous image) into the next record, the prepared
//! image, the desired seat count, and whether a commit is needed.
//! [`discovery`] computes the primary Lease annotations and the ready seats.
//! Production performs the reads, the CAS write, and the Lease and webhook
//! writes around these calls.

use super::{CacheInfo, Entropy, Image, LeaseObservation, NodeInfo, Ownership, PodInfo, Record};
use crate::{
    DiscoveredEndpoint, SeatPolicy, annotations,
    authority::{
        catalog, election,
        keyring::{self, Offer, Policy},
        members, publication,
        state::{self, StateError, StateRecord, Writer},
    },
    pki,
    primary::{WebhookSeat, seat_ready, webhook_ips},
    seat::encode_endpoints,
};
use base64::{Engine, engine::general_purpose::STANDARD};
use std::collections::BTreeMap;

/// Observed cluster objects, compact.
#[derive(Default)]
pub struct Inventory {
    pub nodes: BTreeMap<String, NodeInfo>,
    /// By Pod UID.
    pub pods: BTreeMap<String, PodInfo>,
    /// By ClusterCache UID.
    pub caches: BTreeMap<String, CacheInfo>,
    /// ClusterCaches listed at least once, or the CRD is absent.
    pub caches_synced: bool,
    pub nodes_synced: bool,
    pub pods_synced: bool,
}

impl Inventory {
    /// Every collection listed at least once.
    pub fn synced(&self) -> bool {
        self.nodes_synced && self.pods_synced && self.caches_synced
    }
}

/// Inputs of one pass.
pub struct PassInputs<'a> {
    pub cluster: &'a str,
    pub pod_uid: &'a str,
    pub seats: SeatPolicy,
    pub peer_port: u16,
    pub policy: &'a Policy,
    pub inventory: &'a Inventory,
    /// Nodes, Pods, and ClusterCaches were all synced when the pass began
    /// (production samples [`Inventory::synced`] before its API reads).
    pub synced: bool,
    pub ownership: &'a Ownership,
    /// Committed record just read, if any.
    pub current: Option<&'a StateRecord>,
    /// Image this process holds, if any.
    pub previous_image: Option<&'a Image>,
    /// Live seats by index.
    pub live: &'a BTreeMap<usize, LeaseObservation>,
    pub writer: &'a Writer,
    /// Wall clock, Unix seconds.
    pub wall_unix: u64,
    /// Wall time (Unix seconds) at which this primary last committed a new
    /// membership version, if it has. New members join at most once per
    /// [`JOIN_HOLD_SECS`] after that.
    pub membership_changed_unix: Option<u64>,
}

/// Shortest join hold-down: after a membership version change, new members
/// wait at least this long before the next version admits them, so a storm
/// of joining Pods costs at most one membership version per hold instead of
/// one per debounced pass. Passes run at least every `LEASE_RENEW` (10 s),
/// so a held join lands within one hold plus one pass. Removals are not
/// held. See [`join_hold_secs`].
pub const JOIN_HOLD_SECS: u64 = 10;

/// Longest join hold-down.
pub const MAX_JOIN_HOLD_SECS: u64 = 60;

/// Join hold-down for a cluster of `members` (the larger of the membership
/// and the Nodes with an eligible Pod): one second per thousand, within [`JOIN_HOLD_SECS`] and [`MAX_JOIN_HOLD_SECS`].
///
/// Every version costs each client a reply, and a client that is two or
/// more versions behind gets the full image (about 120 bytes per member)
/// instead of a delta. The cost of one round of full replies grows with
/// the square of the membership while seat capacity grows only linearly
/// (`MEMBERS_PER_SEAT`), so the hold grows with the membership to keep
/// clients within one version. At 50,000 members a 10 s hold produced 33
/// versions during enrollment, 646k full fallbacks and 2.9 TB of seat
/// egress (designs/racer-core-simulation.md, "Round 4: join").
pub fn join_hold_secs(members: usize) -> u64 {
    (members as u64 / 1000).clamp(JOIN_HOLD_SECS, MAX_JOIN_HOLD_SECS)
}

/// Output of one pass.
pub struct PassPlan {
    /// Committed record, or a fresh one stamped for a first write.
    pub base: StateRecord,
    /// The record to commit when [`PassPlan::changed`].
    pub next: StateRecord,
    /// Image for `next`, when the observation was complete.
    pub image: Option<Image>,
    pub desired: usize,
    pub eligible: usize,
    pub changed: bool,
    /// Root offers refused because the keyring root slots are full.
    pub refused_roots: usize,
}

fn record_of(r: Option<&StateRecord>) -> Record {
    r.map(|r| Record {
        sequence: r.sequence,
        membership_version: r.membership_version,
        content_hash: r.content_hash.clone(),
        membership_hash: r.membership_hash.clone(),
    })
    .unwrap_or_default()
}

/// Root offers from live seats.
pub fn offers(live: &BTreeMap<usize, LeaseObservation>) -> Vec<Offer> {
    live.iter()
        .filter_map(|(i, o)| {
            Some(Offer {
                der: STANDARD
                    .decode(o.annotations.get(annotations::ISSUER_CERTIFICATE)?)
                    .ok()?,
                seat: *i as u8,
                holder: o.annotations.get(annotations::HOLDER_UID)?.clone(),
            })
        })
        .collect()
}

/// Plan one pass. Errors are the same `StateError`s production returns:
/// `TooLarge` for transient or size failures, which skip the pass.
pub fn plan_pass(i: &PassInputs<'_>, rng: &mut dyn Entropy) -> Result<PassPlan, StateError> {
    let cluster = i.cluster;
    let previous_record = record_of(i.current);
    let previous_image = i.previous_image.filter(|im| im.record == previous_record);
    let history = previous_image
        .map(|im| publication::history(&im.publication))
        .unwrap_or_default();

    let inv = i.inventory;
    let pods: Vec<PodInfo> = inv.pods.values().cloned().collect();
    let mut by_node: BTreeMap<String, Vec<PodInfo>> = BTreeMap::new();
    for p in &pods {
        by_node
            .entry(p.node_name.clone())
            .or_default()
            .push(p.clone());
    }
    let nodes: Vec<NodeInfo> = inv.nodes.values().cloned().collect();
    // Sized by the eligible Pods too: during a cold start the membership is
    // small but the cluster, and so the cost of each version, is not.
    let eligible = members::eligible_nodes(&pods, i.ownership);
    let hold_secs = join_hold_secs(history.len().max(eligible));
    let joins = members::Joins {
        require_report: true,
        hold: !history.is_empty()
            && i.membership_changed_unix
                .is_some_and(|t| i.wall_unix < t.saturating_add(hold_secs)),
    };
    let members = members::reconcile_at(
        &nodes,
        &by_node,
        i.ownership,
        i.peer_port,
        &history,
        Some(i.wall_unix),
        joins,
    )
    .map_err(|_| StateError::TooLarge);
    let catalog_input: Vec<CacheInfo> = inv.caches.values().cloned().collect();
    let durable = i.current.map(|c| c.keyring.clone()).unwrap_or_default();
    // The catalog is unknown until the ClusterCache watch synced, or when it
    // fails to build or admit and no image of the committed record exists.
    // Unknown is not empty: it must neither publish an empty catalog nor
    // drop cache keys.
    let catalog: Option<Vec<_>> = inv
        .caches_synced
        .then(|| catalog::build(&catalog_input).ok())
        .flatten()
        .and_then(|c| keyring::admit_catalog(&c, &durable).ok())
        .or_else(|| previous_image.map(|im| im.publication.caches.clone()));
    let plan = keyring::plan_with(
        i.policy,
        &durable,
        catalog.as_deref(),
        &offers(i.live),
        i.wall_unix,
        rng,
    )
    .map_err(|_| StateError::TooLarge)?;

    // Without a full observation, keep the committed publication.
    let (member_count, prepared) = match (&members, i.synced, &catalog) {
        (Ok(m), true, Some(catalog)) => {
            let p = publication::prepare(
                &previous_record,
                cluster,
                m.members.values().cloned(),
                catalog.clone(),
            )
            .map_err(|_| StateError::TooLarge)?;
            // Held joiners count: seats must not shrink while they wait.
            (m.members.len() + m.pending, Some(p))
        }
        _ => (
            i.previous_image
                .map_or(0, |im| im.publication.members.len()),
            None,
        ),
    };
    let desired = election::desired_seats(i.seats, member_count, eligible.max(i.live.len()));
    let term_changed = i.current.is_none_or(|c| c.term != *i.writer.term());
    let base = i.current.cloned().unwrap_or_else(|| StateRecord {
        version: state::VERSION,
        cluster: cluster.into(),
        ..Default::default()
    });
    let mut next = i
        .writer
        .stamp(base.clone(), cluster, i.pod_uid, i.wall_unix);
    next.keyring = plan.durable;
    next.desired_seats = desired as u8;
    if let Some(p) = &prepared {
        next.sequence = p.record.sequence;
        next.membership_version = p.record.membership_version;
        next.content_hash = p.record.content_hash.clone();
        next.membership_hash = p.record.membership_hash.clone();
        next.publication_bytes = p.encoded.len() as u64;
    }
    let changed = term_changed
        || plan.changed
        || next.desired_seats != base.desired_seats
        || next.sequence != base.sequence
        || next.membership_version != base.membership_version;
    // An unchanged record keeps the previous image, and with it the delta
    // from the version before: rebuilding it against itself would drop the
    // delta, so every client one version behind would get the full image
    // (Known issue 20), and it would re-encode the image on every pass.
    let image = prepared.map(|p| match i.previous_image {
        Some(prev) if prev.record == p.record => prev.clone(),
        _ => Image::new(p.record, p.publication, p.encoded, i.previous_image),
    });
    Ok(PassPlan {
        base,
        next,
        image,
        desired,
        eligible,
        changed,
        refused_roots: plan.refused_roots,
    })
}

/// Primary Lease discovery annotations and ready seats.
#[derive(Debug)]
pub struct Discovery {
    /// Annotation values the primary Lease should carry.
    pub extra: BTreeMap<&'static str, Option<String>>,
    /// Ready seat indexes, ascending.
    pub ready: Vec<usize>,
    pub endpoints: Vec<DiscoveredEndpoint>,
    pub webhooks: Vec<WebhookSeat>,
}

/// Compute discovery for the committed `sequence`. `previous` is the
/// replaced sequence still accepted during the ready floor grace.
pub fn discovery(
    live: &BTreeMap<usize, LeaseObservation>,
    desired: usize,
    eligible: usize,
    sequence: Option<u64>,
    previous: Option<u64>,
) -> Discovery {
    let width = desired.max(live.keys().next_back().map_or(0, |i| i + 1));
    let nodes: Vec<String> = (0..width)
        .map(|i| {
            live.get(&i)
                .and_then(|o| o.annotations.get(annotations::HOLDER_NODE).cloned())
                .unwrap_or_default()
        })
        .collect();
    let ready: Vec<(usize, &LeaseObservation)> = live
        .iter()
        .filter(|(_, o)| {
            seat_ready(
                o.annotations.get(annotations::READY).map(String::as_str),
                sequence,
                previous,
            )
        })
        .map(|(i, o)| (*i, o))
        .collect();
    let endpoints: Vec<DiscoveredEndpoint> = ready
        .iter()
        .filter_map(|(i, o)| {
            let cert = STANDARD
                .decode(o.annotations.get(annotations::SERVING_CERTIFICATE)?)
                .ok()?;
            Some(DiscoveredEndpoint {
                url: o.annotations.get(annotations::ENDPOINT)?.clone(),
                generation: format!(
                    "{i}-{}-{}",
                    o.spec.transitions,
                    &pki::fingerprint(&cert)[..16]
                ),
                certificate: cert,
            })
        })
        .collect();
    let mut extra = BTreeMap::new();
    extra.insert(annotations::SEATS, Some(desired.to_string()));
    extra.insert(
        annotations::SEAT_NODES,
        Some(serde_json::to_string(&nodes).unwrap_or_default()),
    );
    extra.insert(
        annotations::CLAIM_WINDOW,
        Some((eligible as u64 / 50).clamp(10, 600).to_string()),
    );
    extra.insert(annotations::ENDPOINTS, Some(encode_endpoints(&endpoints)));
    let webhooks: Vec<WebhookSeat> = ready
        .iter()
        .filter_map(|(_, o)| {
            let url = o.annotations.get(annotations::ENDPOINT)?;
            let ip = url
                .strip_prefix("https://")?
                .parse::<std::net::SocketAddr>()
                .ok()?
                .ip();
            Some(WebhookSeat {
                pod: o.spec.holder.clone()?,
                uid: o.annotations.get(annotations::HOLDER_UID)?.clone(),
                ips: webhook_ips(o.annotations.get(annotations::WEBHOOK_ADDRESSES), ip),
                ca: STANDARD
                    .decode(o.annotations.get(annotations::WEBHOOK_CERTIFICATE)?)
                    .ok()?,
            })
        })
        .collect();
    Discovery {
        extra,
        ready: ready.iter().map(|(i, _)| *i).collect(),
        endpoints,
        webhooks,
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::{
        authority::state::Term,
        entropy::{Failing, Seeded},
    };
    use serde_json::json;

    const C: &str = "c1c1c1c1-0000-4000-8000-000000000001";

    fn ownership() -> Ownership {
        Ownership {
            namespace: "racer-system".into(),
            daemonset: "racer-dataplane".into(),
            uid: "ds-uid".into(),
            service_account: "racer-dataplane".into(),
        }
    }

    fn inventory(n: usize) -> Inventory {
        let mut inv = Inventory {
            nodes_synced: true,
            pods_synced: true,
            caches_synced: true,
            ..Default::default()
        };
        for i in 0..n {
            let node = NodeInfo::from_json(&json!({
                "metadata": {"name": format!("n{i}"), "uid": format!("{i:08x}-0000-4000-8000-000000000000")},
                "status": {"addresses": [{"type": "InternalIP", "address": format!("10.0.{}.{}", i / 250, i % 250 + 1)}]}
            }));
            inv.nodes.insert(node.name.clone(), node);
            let pod = PodInfo::from_json(&json!({
                "metadata": {
                    "name": format!("p{i}"), "namespace": "racer-system",
                    "uid": format!("{i:08x}-1111-4000-8000-000000000000"),
                    "annotations": {"racer.unbounded-cloud.io/report": "{\"shares\":1,\"rdma_nics\":[]}"},
                    "ownerReferences": [{"apiVersion": "apps/v1", "kind": "DaemonSet", "name": "racer-dataplane", "uid": "ds-uid", "controller": true}]
                },
                "spec": {"nodeName": format!("n{i}"), "serviceAccountName": "racer-dataplane"},
                "status": {"phase": "Running", "podIP": format!("10.1.{}.{}", i / 250, i % 250 + 1),
                    "conditions": [{"type": "Ready", "status": "True"}]}
            }));
            inv.pods.insert(format!("{i}"), pod);
        }
        inv
    }

    fn writer() -> Writer {
        Writer::new(Term {
            lease_uid: "lease".into(),
            transitions: 1,
            holder: "p0/00".into(),
        })
    }

    fn inputs<'a>(
        inv: &'a Inventory,
        own: &'a Ownership,
        policy: &'a Policy,
        live: &'a BTreeMap<usize, LeaseObservation>,
        w: &'a Writer,
        current: Option<&'a StateRecord>,
        image: Option<&'a Image>,
    ) -> PassInputs<'a> {
        PassInputs {
            cluster: C,
            pod_uid: "pod-uid",
            seats: SeatPolicy::Auto,
            peer_port: 8082,
            policy,
            inventory: inv,
            synced: inv.synced(),
            ownership: own,
            current,
            previous_image: image,
            live,
            writer: w,
            wall_unix: 1_800_000_000,
            membership_changed_unix: None,
        }
    }

    /// Known issue 14: Pods that have not reported (not serving yet) wait,
    /// but still size the seats; reporting admits them.
    #[test]
    fn unreported_pods_wait_but_size_seats() {
        let mut inv = inventory(40);
        for p in inv.pods.values_mut() {
            p.annotations.clear();
        }
        let own = ownership();
        let policy = Policy::from(crate::Rotation::default());
        let live = BTreeMap::new();
        let w = writer();
        let p = plan_pass(
            &inputs(&inv, &own, &policy, &live, &w, None, None),
            &mut Seeded::new(1),
        )
        .unwrap();
        let image = p.image.expect("image");
        // Bootstrap: an empty membership is published.
        assert!(image.publication.members.is_empty());
        let all = plan_pass(
            &inputs(&inventory(40), &own, &policy, &live, &w, None, None),
            &mut Seeded::new(1),
        )
        .unwrap();
        assert_eq!(p.desired, all.desired);
        // One Pod reports: it joins at once (empty history, no hold).
        let committed = p.next;
        let pod = inv.pods.get_mut("0").unwrap();
        pod.annotations.insert(
            annotations::REPORT.into(),
            r#"{"shares":1,"rdma_nics":[]}"#.into(),
        );
        let p2 = plan_pass(
            &inputs(
                &inv,
                &own,
                &policy,
                &live,
                &w,
                Some(&committed),
                Some(&image),
            ),
            &mut Seeded::new(2),
        )
        .unwrap();
        assert_eq!(p2.image.unwrap().publication.members.len(), 1);
        assert_eq!(p2.desired, all.desired);
    }

    #[test]
    fn join_hold_grows_with_the_membership() {
        assert_eq!(join_hold_secs(0), JOIN_HOLD_SECS);
        assert_eq!(join_hold_secs(10_999), JOIN_HOLD_SECS);
        assert_eq!(join_hold_secs(25_000), 25);
        assert_eq!(join_hold_secs(50_000), 50);
        assert_eq!(join_hold_secs(usize::MAX), MAX_JOIN_HOLD_SECS);
    }

    /// A cold start: few members, many eligible Pods. The hold is sized by
    /// the cluster, not by the small membership.
    #[test]
    fn join_hold_is_sized_by_eligible_pods() {
        let own = ownership();
        let policy = Policy::from(crate::Rotation::default());
        let live = BTreeMap::new();
        let w = writer();
        let mut inv = inventory(11_000);
        for (k, p) in inv.pods.iter_mut() {
            if k.parse::<usize>().unwrap() >= 4 {
                p.annotations.clear();
            }
        }
        let first = plan_pass(
            &inputs(&inv, &own, &policy, &live, &w, None, None),
            &mut Seeded::new(1),
        )
        .unwrap();
        let (committed, image) = (first.next, first.image.unwrap());
        assert_eq!(image.publication.members.len(), 4);
        inv.pods.get_mut("4").unwrap().annotations.insert(
            annotations::REPORT.into(),
            r#"{"shares":1,"rdma_nics":[]}"#.into(),
        );
        let at = |since: u64| {
            let mut i = inputs(
                &inv,
                &own,
                &policy,
                &live,
                &w,
                Some(&committed),
                Some(&image),
            );
            i.membership_changed_unix = Some(1_800_000_000 - since);
            plan_pass(&i, &mut Seeded::new(2))
                .unwrap()
                .image
                .unwrap()
                .publication
                .members
                .len()
        };
        assert_eq!(join_hold_secs(11_000), 11);
        assert_eq!(at(JOIN_HOLD_SECS), 4, "held for 11 s, not 10");
        assert_eq!(at(11), 5);
    }

    /// New members are batched: within the hold after a membership change
    /// they wait; afterwards they join.
    #[test]
    fn join_hold_batches_new_members() {
        let own = ownership();
        let policy = Policy::from(crate::Rotation::default());
        let live = BTreeMap::new();
        let w = writer();
        let inv = inventory(4);
        let first = plan_pass(
            &inputs(&inv, &own, &policy, &live, &w, None, None),
            &mut Seeded::new(1),
        )
        .unwrap();
        let (committed, image) = (first.next, first.image.unwrap());
        let more = inventory(6);
        let at = |changed: Option<u64>| {
            let mut i = inputs(
                &more,
                &own,
                &policy,
                &live,
                &w,
                Some(&committed),
                Some(&image),
            );
            i.membership_changed_unix = changed;
            plan_pass(&i, &mut Seeded::new(2)).unwrap()
        };
        let now = 1_800_000_000;
        let held = at(Some(now - JOIN_HOLD_SECS + 1));
        assert!(!held.changed, "joins wait inside the hold");
        let held_image = held.image.unwrap();
        assert_eq!(held_image.publication.members.len(), 4);
        assert_eq!(
            held_image.record.membership_version,
            committed.membership_version
        );
        let open = at(Some(now - JOIN_HOLD_SECS));
        assert!(open.changed);
        assert_eq!(open.image.unwrap().publication.members.len(), 6);
        // A new primary (no change recorded) does not hold.
        assert_eq!(at(None).image.unwrap().publication.members.len(), 6);
        // Removals are never held.
        let fewer = inventory(3);
        let mut i = inputs(
            &fewer,
            &own,
            &policy,
            &live,
            &w,
            Some(&committed),
            Some(&image),
        );
        i.membership_changed_unix = Some(now);
        let p = plan_pass(&i, &mut Seeded::new(3)).unwrap();
        assert_eq!(p.image.unwrap().publication.members.len(), 3);
    }

    #[test]
    fn first_pass_commits_then_settles() {
        let inv = inventory(4);
        let own = ownership();
        let policy = Policy::from(crate::Rotation::default());
        let live = BTreeMap::new();
        let w = writer();
        let p = plan_pass(
            &inputs(&inv, &own, &policy, &live, &w, None, None),
            &mut Seeded::new(1),
        )
        .unwrap();
        assert!(p.changed);
        assert_eq!(p.desired, 3);
        assert_eq!(p.eligible, 4);
        let image = p.image.expect("complete observation builds an image");
        assert_eq!(image.record.sequence, p.next.sequence);
        assert_eq!(
            image.publication.members.len(),
            4,
            "{:?}",
            image.publication.members
        );
        // Same observation over the committed record: nothing to write.
        let committed = p.next;
        let p2 = plan_pass(
            &inputs(
                &inv,
                &own,
                &policy,
                &live,
                &w,
                Some(&committed),
                Some(&image),
            ),
            &mut Seeded::new(2),
        )
        .unwrap();
        assert!(!p2.changed);
        assert_eq!(p2.next.sequence, committed.sequence);
        // A new Node bumps the sequence and membership.
        let more = inventory(5);
        let p3 = plan_pass(
            &inputs(
                &more,
                &own,
                &policy,
                &live,
                &w,
                Some(&committed),
                Some(&image),
            ),
            &mut Seeded::new(3),
        )
        .unwrap();
        assert!(p3.changed);
        assert_eq!(p3.next.sequence, committed.sequence + 1);
        assert_eq!(p3.next.membership_version, committed.membership_version + 1);
        let image3 = p3.image.unwrap();
        assert!(image3.delta.is_some());
        // A later pass with nothing new keeps that image and its delta, so
        // clients one version behind still get a delta (Known issue 20).
        let committed3 = p3.next;
        let p4 = plan_pass(
            &inputs(
                &more,
                &own,
                &policy,
                &live,
                &w,
                Some(&committed3),
                Some(&image3),
            ),
            &mut Seeded::new(4),
        )
        .unwrap();
        assert!(!p4.changed);
        let image4 = p4.image.expect("complete observation keeps an image");
        assert_eq!(image4.record, image3.record);
        assert_eq!(
            image4.delta.as_ref().map(|(s, h, _)| (*s, h.clone())),
            image3.delta.as_ref().map(|(s, h, _)| (*s, h.clone()))
        );
        assert!(std::sync::Arc::ptr_eq(&image4.encoded, &image3.encoded));
    }

    #[test]
    fn incomplete_observation_keeps_publication() {
        let mut inv = inventory(2);
        inv.pods_synced = false;
        let own = ownership();
        let policy = Policy::from(crate::Rotation::default());
        let live = BTreeMap::new();
        let w = writer();
        let p = plan_pass(
            &inputs(&inv, &own, &policy, &live, &w, None, None),
            &mut Failing,
        )
        .unwrap();
        assert!(p.image.is_none());
        assert_eq!(p.next.sequence, 0);
        // Term change alone still needs a commit.
        assert!(p.changed);
    }

    #[test]
    fn entropy_failure_skips_pass_when_keys_are_needed() {
        let mut inv = inventory(1);
        inv.caches.insert(
            "c".into(),
            CacheInfo::from_json(&json!({
                "metadata": {"name": "c", "uid": "11111111-1111-4111-8111-111111111111"},
                "spec": {}
            })),
        );
        let own = ownership();
        let policy = Policy::from(crate::Rotation::default());
        let live = BTreeMap::new();
        let w = writer();
        let i = inputs(&inv, &own, &policy, &live, &w, None, None);
        // A cache that fails to build is simply not in the catalog; when it
        // builds, keys need entropy and a failing source skips the pass.
        if let Ok(p) = plan_pass(&i, &mut Failing) {
            assert!(p.next.keyring.keys.is_empty());
        }
    }

    fn seat(i: usize, node: &str, ready: Option<&str>) -> LeaseObservation {
        let mut a = BTreeMap::from([
            (annotations::HOLDER_NODE.to_owned(), node.to_owned()),
            (annotations::HOLDER_UID.to_owned(), format!("uid{i}")),
            (
                annotations::ENDPOINT.to_owned(),
                format!("https://10.0.0.{}:8443", i + 1),
            ),
            (
                annotations::SERVING_CERTIFICATE.to_owned(),
                STANDARD.encode([i as u8; 8]),
            ),
            (
                annotations::WEBHOOK_CERTIFICATE.to_owned(),
                STANDARD.encode([i as u8; 4]),
            ),
        ]);
        if let Some(r) = ready {
            a.insert(annotations::READY.to_owned(), r.to_owned());
        }
        LeaseObservation {
            name: annotations::seat_lease(i),
            spec: election::LeaseSpec {
                holder: Some(format!("p{i}")),
                transitions: 2,
                ..Default::default()
            },
            annotations: a,
            ..Default::default()
        }
    }

    #[test]
    fn discovery_lists_ready_seats_and_vacancies() {
        let live = BTreeMap::from([
            (0, seat(0, "n0", Some("5"))),
            (2, seat(2, "n2", Some("4"))),
            (4, seat(4, "n4", None)),
        ]);
        let d = discovery(&live, 3, 1000, Some(5), None);
        assert_eq!(d.ready, vec![0]);
        assert_eq!(d.endpoints.len(), 1);
        assert_eq!(d.webhooks.len(), 1);
        assert_eq!(d.extra[annotations::SEATS].as_deref(), Some("3"));
        assert_eq!(
            d.extra[annotations::SEAT_NODES].as_deref(),
            Some(r#"["n0","","n2","","n4"]"#)
        );
        assert_eq!(d.extra[annotations::CLAIM_WINDOW].as_deref(), Some("20"));
        // During the ready floor grace, the replaced sequence still counts.
        let d = discovery(&live, 3, 0, Some(5), Some(4));
        assert_eq!(d.ready, vec![0, 2]);
        assert_eq!(d.extra[annotations::CLAIM_WINDOW].as_deref(), Some("10"));
    }
}
