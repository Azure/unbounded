//! Select RDMA only for same-Site hops with compatible authenticated mappings.
//! Published Node-annotation mappings must match local hardware; discovery never
//! reports or overrides membership. Missing/incompatible mappings fall back to HTTP.
use super::{
    hash,
    paths::{FAILURE_LINKS, Route},
};
use crate::{
    error::{Error, Result},
    model::PageId,
};
#[derive(Clone, Copy, Debug, Eq, Ord, PartialEq, PartialOrd)]
pub struct RailId(pub u16);
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct RailMapping {
    pub rail: RailId,
    pub fabric: String,
    pub numa_node: Option<usize>,
}
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum TransportPlan {
    Http,
    Rdma { rail: RailId },
}
/// Conservative whole-route summary; transport admission must use `select_hop`.
/// Select from authenticated advertised mappings. Production session admission
/// also requires device activation and readiness against the local publication;
/// hardware discovery may only veto this plan, never replace it.
pub fn select(route: &Route, page: &PageId) -> Result<TransportPlan> {
    validate_route(route)?;
    let members = route
        .nodes
        .iter()
        .map(|node| route.membership.member(node))
        .collect::<Result<Vec<_>>>()?;
    select_members(&route.membership, &members, page)
}

/// Select the actual immediate hop, independently of other hops' Sites or rails.
/// Both identities must be adjacent in the authenticated route. Hardware discovery
/// still only vetoes the selected published rail before native session admission.
pub fn select_hop(
    route: &Route,
    page: &PageId,
    local: &crate::model::NodeId,
    peer: &crate::model::NodeId,
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
    membership: &super::membership::Membership,
    members: &[&super::membership::Member],
    page: &PageId,
) -> Result<TransportPlan> {
    if members.iter().any(|member| {
        member.site.is_empty()
            || member.site != members[0].site
            || !member.alignment_enabled
            || member.rails.is_empty()
    }) {
        return Ok(TransportPlan::Http);
    }
    // Keep the publication-wide domain and v2 hash stable. Site is an admission
    // boundary, not a new rail numbering or page placement scheme.
    let domain = membership.rail_domain();
    if domain.is_empty() {
        return Ok(TransportPlan::Http);
    }
    let mut digest = hash::domain(b"racer/rail/v2\0");
    hash::object(&mut digest, &page.version.object, page.number);
    let digest = hash::finish(digest);
    let sample = u64::from_be_bytes(digest[..8].try_into().unwrap());
    let rail = domain[(sample % domain.len() as u64) as usize];
    let Some(chosen) = members[0].rails.iter().find(|m| m.rail == rail) else {
        return Ok(TransportPlan::Http);
    };
    if !members[1..].iter().all(|member| {
        member
            .rails
            .iter()
            .any(|m| m.rail == rail && m.fabric == chosen.fabric)
    }) {
        return Ok(TransportPlan::Http);
    }
    Ok(TransportPlan::Rdma { rail })
}

pub fn select_with_local(
    route: &Route,
    page: &PageId,
    local: &crate::model::NodeId,
    discovered: &[RailMapping],
) -> Result<TransportPlan> {
    let plan = select(route, page)?;
    if local_compatible(route, &plan, local, discovered)? {
        Ok(plan)
    } else {
        Ok(TransportPlan::Http)
    }
}

/// All participants must confirm the selected rail before payload admission.
/// NUMA IDs are local, so they match hardware locally, not between nodes.
pub fn local_compatible(
    route: &Route,
    plan: &TransportPlan,
    local: &crate::model::NodeId,
    discovered: &[RailMapping],
) -> Result<bool> {
    if !route.nodes.contains(local) {
        return Err(Error::IncompatibleMembership);
    }
    let member = route.membership.member(local)?;
    let TransportPlan::Rdma { rail } = plan else {
        return Ok(true);
    };
    if !member.alignment_enabled {
        return Ok(false);
    }
    let Some(published) = member.rails.iter().find(|mapping| mapping.rail == *rail) else {
        return Ok(false);
    };
    let mut matching = discovered.iter().filter(|mapping| mapping.rail == *rail);
    let Some(hardware) = matching.next() else {
        return Ok(false);
    };
    Ok(matching.next().is_none()
        && hardware.fabric == published.fabric
        && published
            .numa_node
            .is_none_or(|numa| hardware.numa_node == Some(numa)))
}
#[cfg(test)]
mod tests {
    use super::*;
    use crate::{
        model::*,
        topology::{
            fixtures::{member, object},
            membership::Membership,
        },
    };
    use std::sync::Arc;

    fn mappings() -> Vec<RailMapping> {
        vec![
            RailMapping {
                rail: RailId(7),
                fabric: "a".into(),
                numa_node: Some(0),
            },
            RailMapping {
                rail: RailId(2),
                fabric: "b".into(),
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

    #[test]
    fn mixed_site_hops_preserve_global_rail_mapping_and_hardware_vetoes() {
        let mixed = route(|m| m[2].site = "site2".into());
        let a = &mixed.nodes[0];
        let b = &mixed.nodes[1];
        let c = &mixed.nodes[2];
        assert_eq!(select(&mixed, &page(0)).unwrap(), TransportPlan::Http);
        assert_eq!(
            select_hop(&mixed, &page(0), a, b).unwrap(),
            TransportPlan::Rdma { rail: RailId(2) }
        );
        assert_eq!(
            select_hop(&mixed, &page(0), b, a).unwrap(),
            TransportPlan::Rdma { rail: RailId(2) }
        );
        assert_eq!(
            select_hop(&mixed, &page(0), b, c).unwrap(),
            TransportPlan::Http
        );
        assert_eq!(
            select_hop(&mixed, &page(0), c, b).unwrap(),
            TransportPlan::Http
        );
        assert!(select_hop(&mixed, &page(0), a, c).is_err());
        assert!(select_hop(&mixed, &page(0), a, a).is_err());
        assert!(select_hop(&mixed, &page(0), a, &NodeId("unknown".into())).is_err());
        for local in [0, 1] {
            let missing = route(|m| m[local].site.clear());
            assert_eq!(
                select_hop(&missing, &page(0), a, b).unwrap(),
                TransportPlan::Http
            );
        }
        for changed in [
            route(|m| m[1].alignment_enabled = false),
            route(|m| m[1].rails.clear()),
            route(|m| {
                m[1].rails
                    .iter_mut()
                    .for_each(|r| r.fabric = "wrong".into())
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
        for (number, rail) in [(0, 2), (1, 7), (u64::MAX, 2)] {
            assert_eq!(
                select(&route, &page(number)).unwrap(),
                TransportPlan::Rdma { rail: RailId(rail) }
            );
        }
    }
    fn route(change: impl FnOnce(&mut Vec<super::super::membership::Member>)) -> Route {
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
    fn intersection_over_all_hops_and_http_fallback() {
        for route in [
            route(|m| m[1].alignment_enabled = false),
            route(|m| m[1].rails.clear()),
            route(|m| {
                for rail in &mut m[1].rails {
                    rail.fabric = "wrong".into();
                }
            }),
        ] {
            assert_eq!(select(&route, &page(0)).unwrap(), TransportPlan::Http);
        }
        let full = route(|_| {});
        let partial = route(|m| m[1].rails.retain(|rail| rail.rail == RailId(7)));
        for number in 0..100 {
            let expected = match select(&full, &page(number)).unwrap() {
                TransportPlan::Rdma { rail: RailId(7) } => TransportPlan::Rdma { rail: RailId(7) },
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
            assert_eq!(
                select_with_local(&route, &page(number), &route.nodes[0], &mappings()).unwrap(),
                plan
            );
            assert_eq!(
                select_with_local(&route, &page(number), &route.nodes[1], &mappings()).unwrap(),
                TransportPlan::Http
            );
            assert_eq!(
                select_with_local(&route, &page(number), &route.nodes[0], &[]).unwrap(),
                TransportPlan::Http
            );
        }
        assert_eq!(selected.len(), 2);
        let mut invalid = route.clone();
        invalid.nodes.push(invalid.nodes[0].clone());
        assert_eq!(select(&invalid, &page(0)), Err(Error::InvalidRequest));
    }
}
