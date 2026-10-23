//! Membership from Nodes and managed dataplane Pods.
//!
//! Follows the membership rules of the removed Go controller with one change: a Pod reports
//! its own shares and NICs in the `racer.unbounded-cloud.io/report`
//! annotation. Administrator Node annotations win over the report, and the
//! report wins over defaults. Seats never write Nodes.

use super::rails;
use crate::{annotations, kube};
use racer_control_wire::{self as wire, Member, NodeId, RailMapping};
use serde_json::Value;
use std::{
    collections::{BTreeMap, HashSet},
    net::IpAddr,
    num::NonZeroU32,
};

/// Node fields used for membership.
#[derive(Clone, Debug, Default)]
pub struct NodeInfo {
    pub name: String,
    pub uid: String,
    pub labels: BTreeMap<String, String>,
    pub annotations: BTreeMap<String, String>,
    pub deleting: bool,
}

/// Controller owner reference.
#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct Owner {
    pub api_version: String,
    pub kind: String,
    pub name: String,
    pub uid: String,
}

/// Pod fields used for membership and authorization.
#[derive(Clone, Debug, Default)]
pub struct PodInfo {
    pub namespace: String,
    #[allow(dead_code)]
    pub name: String,
    pub uid: String,
    pub node_name: String,
    pub pod_ip: String,
    pub phase: String,
    pub deleting: bool,
    pub creation: String,
    pub service_account: String,
    pub owner: Option<Owner>,
    pub annotations: BTreeMap<String, String>,
}

impl NodeInfo {
    pub fn from_json(v: &Value) -> Self {
        Self {
            name: kube::str_at(v, &["metadata", "name"]).into(),
            uid: kube::str_at(v, &["metadata", "uid"]).into(),
            labels: kube::map_at(v, &["metadata", "labels"]),
            annotations: kube::map_at(v, &["metadata", "annotations"]),
            deleting: !kube::str_at(v, &["metadata", "deletionTimestamp"]).is_empty(),
        }
    }

    /// Eligible to be a member or to enroll.
    pub fn authorized(&self) -> bool {
        !self.name.is_empty()
            && wire::valid_uuid(&self.uid)
            && !self.deleting
            && !self.labels.contains_key(annotations::EXCLUDE)
    }
}

impl PodInfo {
    pub fn from_json(v: &Value) -> Self {
        let owner = v["metadata"]["ownerReferences"]
            .as_array()
            .and_then(|a| a.iter().find(|o| o["controller"].as_bool() == Some(true)))
            .map(|o| Owner {
                api_version: kube::str_at(o, &["apiVersion"]).into(),
                kind: kube::str_at(o, &["kind"]).into(),
                name: kube::str_at(o, &["name"]).into(),
                uid: kube::str_at(o, &["uid"]).into(),
            });
        Self {
            namespace: kube::str_at(v, &["metadata", "namespace"]).into(),
            name: kube::str_at(v, &["metadata", "name"]).into(),
            uid: kube::str_at(v, &["metadata", "uid"]).into(),
            node_name: kube::str_at(v, &["spec", "nodeName"]).into(),
            pod_ip: kube::str_at(v, &["status", "podIP"]).into(),
            phase: kube::str_at(v, &["status", "phase"]).into(),
            deleting: !kube::str_at(v, &["metadata", "deletionTimestamp"]).is_empty(),
            creation: kube::str_at(v, &["metadata", "creationTimestamp"]).into(),
            service_account: kube::str_at(v, &["spec", "serviceAccountName"]).into(),
            owner,
            annotations: kube::map_at(v, &["metadata", "annotations"]),
        }
    }
}

/// The live managed DaemonSet. An empty UID owns nothing.
#[derive(Clone, Debug, Default)]
pub struct Ownership {
    pub namespace: String,
    pub daemonset: String,
    pub uid: String,
    pub service_account: String,
}

impl Ownership {
    /// Whether the managed DaemonSet controls this live Pod.
    pub fn owns(&self, pod: &PodInfo) -> bool {
        let Some(o) = &pod.owner else { return false };
        !self.uid.is_empty()
            && pod.namespace == self.namespace
            && o.api_version == "apps/v1"
            && o.kind == "DaemonSet"
            && o.name == self.daemonset
            && o.uid == self.uid
    }

    /// Managed, live, and running as the dataplane service account.
    pub fn manages(&self, pod: &PodInfo) -> bool {
        self.owns(pod)
            && !pod.uid.is_empty()
            && !pod.deleting
            && !pod.node_name.is_empty()
            && pod.service_account == self.service_account
            && pod.phase != "Failed"
            && pod.phase != "Succeeded"
    }
}

/// Address of an eligible Pod on `node`.
fn endpoint_ip(pod: &PodInfo, ownership: &Ownership, node: &str) -> Option<IpAddr> {
    if pod.node_name != node || !ownership.manages(pod) {
        return None;
    }
    let ip: IpAddr = pod.pod_ip.parse().ok()?;
    Some(ip)
}

/// Newest eligible Pod on `node`, ties by UID.
pub fn select_pod<'a>(
    pods: &'a [PodInfo],
    ownership: &Ownership,
    node: &str,
) -> Option<(&'a PodInfo, IpAddr)> {
    let mut best: Option<(&PodInfo, IpAddr)> = None;
    for p in pods {
        if let Some(ip) = endpoint_ip(p, ownership, node)
            && best.is_none_or(|(b, _)| {
                (p.creation.as_str(), p.uid.as_str()) > (b.creation.as_str(), b.uid.as_str())
            })
        {
            best = Some((p, ip));
        }
    }
    best
}

/// Previously published members.
pub type History = BTreeMap<String, Member>;

/// One membership input diagnostic.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct Diagnostic {
    pub object: String,
    pub field: &'static str,
}

/// Membership candidate.
#[derive(Debug, Default)]
pub struct Result {
    pub members: History,
    pub diagnostics: Vec<Diagnostic>,
}

/// Membership failures.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum MembersError {
    Invalid,
    TooLarge,
}

struct Attributes {
    shares: NonZeroU32,
    rails: Vec<RailMapping>,
}

fn attributes(
    node: &NodeInfo,
    report: Option<&str>,
) -> std::result::Result<Attributes, &'static str> {
    let mut out = Attributes {
        shares: NonZeroU32::new(wire::DEFAULT_SHARES).expect("nonzero"),
        rails: Vec::new(),
    };
    if let Some(r) = report {
        let r = rails::parse_report(r).map_err(|_| "report")?;
        out.shares = r.shares;
        out.rails = r.rdma_nics;
    }
    if let Some(v) = node.annotations.get(annotations::SHARES) {
        if v.is_empty() || v.starts_with('+') || !v.bytes().all(|b| b.is_ascii_digit()) {
            return Err("shares");
        }
        out.shares = v
            .parse::<u32>()
            .ok()
            .and_then(NonZeroU32::new)
            .ok_or("shares")?;
    }
    if let Some(v) = node.annotations.get(annotations::RDMA_NICS) {
        out.rails = rails::parse_nics_str(v).map_err(|_| "rdma_nics")?;
    }
    Ok(out)
}

/// Build membership. `pods_by_node` groups Pods by `spec.nodeName`.
pub fn reconcile(
    nodes: &[NodeInfo],
    pods_by_node: &BTreeMap<String, Vec<PodInfo>>,
    ownership: &Ownership,
    peer_port: u16,
    accepted: &History,
) -> std::result::Result<Result, MembersError> {
    if peer_port == 0 {
        return Err(MembersError::Invalid);
    }
    let mut nodes: Vec<&NodeInfo> = nodes.iter().collect();
    nodes.sort_by(|a, b| a.uid.cmp(&b.uid));
    let mut result = Result::default();
    let (mut ids, mut names) = (HashSet::new(), HashSet::new());
    let empty = Vec::new();
    for node in nodes {
        if !wire::valid_uuid(&node.uid)
            || node.name.is_empty()
            || !ids.insert(&node.uid)
            || !names.insert(&node.name)
        {
            return Err(MembersError::Invalid);
        }
        if !node.authorized() {
            continue;
        }
        let previous = accepted.get(&node.uid);
        let pods = pods_by_node.get(&node.name).unwrap_or(&empty);
        let selected = select_pod(pods, ownership, &node.name);
        let report = selected
            .and_then(|(p, _)| p.annotations.get(annotations::REPORT))
            .map(String::as_str);
        let attrs = match attributes(node, report) {
            Ok(a) => Some(a),
            Err(field) => {
                result.diagnostics.push(Diagnostic {
                    object: node.name.clone(),
                    field,
                });
                previous.map(|p| Attributes {
                    shares: p.shares,
                    rails: p.rails.clone(),
                })
            }
        };
        let endpoint = match selected {
            Some((_, ip)) => Some(std::net::SocketAddr::new(ip, peer_port).to_string()),
            None => {
                result.diagnostics.push(Diagnostic {
                    object: node.name.clone(),
                    field: "peer_endpoint",
                });
                previous.map(|p| p.peer_endpoint.clone())
            }
        };
        let (Some(attrs), Some(endpoint)) = (attrs, endpoint) else {
            continue;
        };
        let site = node
            .labels
            .get(annotations::SITE)
            .cloned()
            .unwrap_or_default();
        let mut rails = attrs.rails;
        if wire::normalize_rails(&mut rails).is_err()
            || wire::validate_member_metadata(&endpoint, &site).is_err()
        {
            result.diagnostics.push(Diagnostic {
                object: node.name.clone(),
                field: "member",
            });
            if let Some(p) = previous {
                result.members.insert(node.uid.clone(), p.clone());
            }
            continue;
        }
        result.members.insert(
            node.uid.clone(),
            Member {
                node: NodeId(node.uid.clone()),
                shares: attrs.shares,
                peer_endpoint: endpoint,
                rails,
                site,
            },
        );
    }
    if result.members.len() > wire::MAX_MEMBERS {
        return Err(MembersError::TooLarge);
    }
    Ok(result)
}

/// Distinct Nodes with an eligible Pod, for seat sizing.
pub fn eligible_nodes(pods: &[PodInfo], ownership: &Ownership) -> usize {
    pods.iter()
        .filter(|p| ownership.manages(p) && p.pod_ip.parse::<IpAddr>().is_ok())
        .map(|p| p.node_name.as_str())
        .collect::<HashSet<_>>()
        .len()
}

#[cfg(test)]
mod tests {
    use super::*;

    const DS: &str = "dddddddd-dddd-4ddd-8ddd-dddddddddddd";

    fn own() -> Ownership {
        Ownership {
            namespace: "ns".into(),
            daemonset: "racer-dataplane".into(),
            uid: DS.into(),
            service_account: "racer-dataplane".into(),
        }
    }

    fn node(n: u8) -> NodeInfo {
        NodeInfo {
            name: format!("n{n}"),
            uid: format!("{n:08x}-0000-4000-8000-000000000000"),
            ..Default::default()
        }
    }

    fn pod(node: &str, ip: &str, created: &str, uid: &str) -> PodInfo {
        PodInfo {
            namespace: "ns".into(),
            name: format!("p-{uid}"),
            uid: uid.into(),
            node_name: node.into(),
            pod_ip: ip.into(),
            phase: "Running".into(),
            creation: created.into(),
            service_account: "racer-dataplane".into(),
            owner: Some(Owner {
                api_version: "apps/v1".into(),
                kind: "DaemonSet".into(),
                name: "racer-dataplane".into(),
                uid: DS.into(),
            }),
            ..Default::default()
        }
    }

    fn by_node(pods: Vec<PodInfo>) -> BTreeMap<String, Vec<PodInfo>> {
        let mut m: BTreeMap<String, Vec<PodInfo>> = BTreeMap::new();
        for p in pods {
            m.entry(p.node_name.clone()).or_default().push(p);
        }
        m
    }

    #[test]
    fn precedence_admin_over_report_over_default() {
        let mut a = node(1);
        let b = node(2);
        let mut pa = pod("n1", "10.0.0.1", "2024-01-01T00:00:00Z", "a");
        pa.annotations.insert(
            annotations::REPORT.into(),
            r#"{"shares":7,"rdma_nics":[{"device":"d","port":1,"rail":3}]}"#.into(),
        );
        let mut pb = pod("n2", "10.0.0.2", "2024-01-01T00:00:00Z", "b");
        pb.annotations.insert(
            annotations::REPORT.into(),
            r#"{"shares":9,"rdma_nics":[]}"#.into(),
        );
        a.annotations.insert(annotations::SHARES.into(), "2".into());
        a.labels.insert(annotations::SITE.into(), "west".into());
        let r = reconcile(
            &[a.clone(), b.clone(), node(3)],
            &by_node(vec![pa, pb]),
            &own(),
            8082,
            &History::new(),
        )
        .unwrap();
        let ma = &r.members[&a.uid];
        assert_eq!(ma.shares.get(), 2);
        assert_eq!(ma.rails.len(), 1);
        assert_eq!(ma.site, "west");
        assert_eq!(ma.peer_endpoint, "10.0.0.1:8082");
        assert_eq!(r.members[&b.uid].shares.get(), 9);
        // Node 3 has no Pod and no history.
        assert_eq!(r.members.len(), 2);
        assert!(r.diagnostics.iter().any(|d| d.object == "n3"));
    }

    #[test]
    fn maximum_report_preserved_in_membership() {
        let a = node(1);
        let report = rails::tests::max_report();
        let mut p = pod("n1", "10.0.0.1", "2024-01-01T00:00:00Z", "a");
        p.annotations.insert(
            annotations::REPORT.into(),
            rails::encode_report(&report).unwrap(),
        );
        let r = reconcile(
            std::slice::from_ref(&a),
            &by_node(vec![p]),
            &own(),
            1,
            &History::new(),
        )
        .unwrap();
        assert!(r.diagnostics.iter().all(|d| d.field != "report"));
        let m = &r.members[&a.uid];
        assert_eq!(m.shares, report.shares);
        assert_eq!(m.rails, report.rdma_nics);
    }

    #[test]
    fn history_survives_gaps_and_bad_input() {
        let a = node(1);
        let p = pod("n1", "10.0.0.1", "t1", "a");
        let first = reconcile(
            std::slice::from_ref(&a),
            &by_node(vec![p.clone()]),
            &own(),
            1,
            &History::new(),
        )
        .unwrap();
        // Pod gone: keep previous endpoint.
        let gap = reconcile(
            std::slice::from_ref(&a),
            &BTreeMap::new(),
            &own(),
            1,
            &first.members,
        )
        .unwrap();
        assert_eq!(gap.members[&a.uid].peer_endpoint, "10.0.0.1:1");
        // Malformed report: keep previous attributes, new endpoint.
        let mut bad = pod("n1", "10.0.0.9", "t2", "b");
        bad.annotations
            .insert(annotations::REPORT.into(), "{".into());
        let r = reconcile(
            std::slice::from_ref(&a),
            &by_node(vec![bad.clone()]),
            &own(),
            1,
            &first.members,
        )
        .unwrap();
        assert_eq!(r.members[&a.uid].shares.get(), wire::DEFAULT_SHARES);
        assert_eq!(r.members[&a.uid].peer_endpoint, "10.0.0.9:1");
        // Never admitted with bad input: omitted.
        let r = reconcile(&[a], &by_node(vec![bad]), &own(), 1, &History::new()).unwrap();
        assert!(r.members.is_empty());
    }

    #[test]
    fn ownership_exclusion_and_selection() {
        let mut a = node(1);
        let old = pod("n1", "10.0.0.1", "2024-01-01T00:00:00Z", "a");
        let new = pod("n1", "10.0.0.2", "2024-01-02T00:00:00Z", "b");
        let mut foreign = pod("n1", "10.0.0.3", "2025-01-01T00:00:00Z", "c");
        foreign.owner.as_mut().unwrap().uid = "other".into();
        let mut dying = pod("n1", "10.0.0.4", "2026-01-01T00:00:00Z", "d");
        dying.deleting = true;
        let mut wrong_sa = pod("n1", "10.0.0.5", "2026-01-01T00:00:00Z", "e");
        wrong_sa.service_account = "x".into();
        let pods = vec![old, new, foreign, dying, wrong_sa];
        let r = reconcile(
            std::slice::from_ref(&a),
            &by_node(pods.clone()),
            &own(),
            1,
            &History::new(),
        )
        .unwrap();
        assert_eq!(r.members[&a.uid].peer_endpoint, "10.0.0.2:1");
        assert_eq!(eligible_nodes(&pods, &own()), 1);
        a.labels.insert(annotations::EXCLUDE.into(), String::new());
        let r = reconcile(
            std::slice::from_ref(&a),
            &by_node(pods.clone()),
            &own(),
            1,
            &History::new(),
        )
        .unwrap();
        assert!(r.members.is_empty());
        let none = Ownership {
            uid: String::new(),
            ..own()
        };
        assert_eq!(eligible_nodes(&pods, &none), 0);
    }

    #[test]
    fn duplicate_identity_fails() {
        let a = node(1);
        assert_eq!(
            reconcile(
                &[a.clone(), a],
                &BTreeMap::new(),
                &own(),
                1,
                &History::new()
            )
            .unwrap_err(),
            MembersError::Invalid
        );
        assert_eq!(
            reconcile(&[], &BTreeMap::new(), &own(), 0, &History::new()).unwrap_err(),
            MembersError::Invalid
        );
    }

    #[test]
    fn json_parsing() {
        let v = serde_json::json!({
            "metadata": {"name":"p","namespace":"ns","uid":"u","creationTimestamp":"t",
                "ownerReferences":[{"apiVersion":"apps/v1","kind":"DaemonSet","name":"racer-dataplane","uid":DS,"controller":true}]},
            "spec": {"nodeName":"n1","serviceAccountName":"racer-dataplane"},
            "status": {"podIP":"10.1.1.1","phase":"Running"}
        });
        let p = PodInfo::from_json(&v);
        assert!(own().manages(&p));
        assert_eq!(p.pod_ip, "10.1.1.1");
        let n = NodeInfo::from_json(
            &serde_json::json!({"metadata":{"name":"n","uid":"x","deletionTimestamp":"t"}}),
        );
        assert!(n.deleting && !n.authorized());
    }
}
