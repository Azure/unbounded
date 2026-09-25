//! Select RDMA only with compatible authenticated mappings over the entire path.
//! Published Node-annotation mappings must match local hardware; discovery never
//! reports or overrides membership. Missing/incompatible mappings fall back to HTTP.
use super::{
    hash,
    paths::{FAILURE_LINKS, Route},
};
use crate::{
    error::{Error, Result},
    model::identity::PageId,
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
pub struct Rails;
impl Rails {
    /// Select from authenticated advertised mappings. Before creating an RDMA
    /// session, validate each hop's actual hardware with `select_with_local` or
    /// `local_compatible`; discovery may only veto this plan, never replace it.
    pub fn select(&self, route: &Route, page: &PageId) -> Result<TransportPlan> {
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
        let members = route
            .nodes
            .iter()
            .map(|node| route.membership.member(node))
            .collect::<Result<Vec<_>>>()?;
        if members
            .iter()
            .any(|member| !member.alignment_enabled || member.rails.is_empty())
        {
            return Ok(TransportPlan::Http);
        }
        let compatible: Vec<_> = members[0]
            .rails
            .iter()
            .filter(|mapping| {
                members[1..].iter().all(|member| {
                    member
                        .rails
                        .iter()
                        .any(|other| mapping.rail == other.rail && mapping.fabric == other.fabric)
                })
            })
            .collect();
        if compatible.is_empty() {
            return Ok(TransportPlan::Http);
        }
        let mut digest = hash::domain(b"racer/rail/v1\0");
        hash::object(&mut digest, &page.version.object, page.number);
        hash::bytes(&mut digest, page.version.etag.as_bytes());
        let digest = hash::finish(digest);
        let sample = u64::from_be_bytes(digest[..8].try_into().unwrap());
        let chosen = compatible[(sample % compatible.len() as u64) as usize];
        Ok(TransportPlan::Rdma { rail: chosen.rail })
    }

    pub fn select_with_local(
        &self,
        route: &Route,
        page: &PageId,
        local: &crate::model::identity::NodeId,
        discovered: &[RailMapping],
    ) -> Result<TransportPlan> {
        let plan = self.select(route, page)?;
        if self.local_compatible(route, &plan, local, discovered)? {
            Ok(plan)
        } else {
            Ok(TransportPlan::Http)
        }
    }

    /// All participants must confirm the selected rail before payload admission.
    /// NUMA IDs are local, so they match hardware locally, not between nodes.
    pub fn local_compatible(
        &self,
        route: &Route,
        plan: &TransportPlan,
        local: &crate::model::identity::NodeId,
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
}
#[cfg(test)]
mod tests {
    use super::*;
    use crate::{
        model::identity::*,
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
    fn golden_page_to_rail_vectors() {
        let route = route(|_| {});
        for (number, rail) in [(0, 2), (1, 7), (u64::MAX, 7)] {
            assert_eq!(
                Rails.select(&route, &page(number)).unwrap(),
                TransportPlan::Rdma { rail: RailId(rail) }
            );
        }
    }
    fn route(change: impl FnOnce(&mut Vec<super::super::membership::Member>)) -> Route {
        let mut members: Vec<_> = (0..3)
            .map(|i| {
                let mut member = member(i, 4);
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
            assert_eq!(Rails.select(&route, &page(0)).unwrap(), TransportPlan::Http);
        }
        let route = route(|m| m[1].rails.retain(|rail| rail.rail == RailId(7)));
        assert_eq!(
            Rails.select(&route, &page(0)).unwrap(),
            TransportPlan::Rdma { rail: RailId(7) }
        );
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
            let plan = Rails.select(&route, &page(number)).unwrap();
            assert_eq!(plan, Rails.select(&reverse, &page(number)).unwrap());
            let TransportPlan::Rdma { rail } = plan else {
                panic!("expected RDMA");
            };
            selected.insert(rail);
            assert_eq!(
                Rails
                    .select_with_local(&route, &page(number), &route.nodes[0], &mappings())
                    .unwrap(),
                plan
            );
            assert_eq!(
                Rails
                    .select_with_local(&route, &page(number), &route.nodes[1], &mappings())
                    .unwrap(),
                TransportPlan::Http
            );
            assert_eq!(
                Rails
                    .select_with_local(&route, &page(number), &route.nodes[0], &[])
                    .unwrap(),
                TransportPlan::Http
            );
        }
        assert_eq!(selected.len(), 2);
        let mut invalid = route.clone();
        invalid.nodes.push(invalid.nodes[0].clone());
        assert_eq!(Rails.select(&invalid, &page(0)), Err(Error::InvalidRequest));
    }
}
