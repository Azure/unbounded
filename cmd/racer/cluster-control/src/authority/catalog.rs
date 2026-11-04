//! Cache catalog from ClusterCaches, like members.BuildCatalog in the old controller.

use super::pools::Selector;
use crate::kube;
use racer_control_wire::{self as wire, CacheDefinition, CacheId};
use serde_json::Value;
use std::collections::{BTreeMap, HashSet};

/// ClusterCache fields used here.
#[derive(Clone, Debug, Default)]
pub struct CacheInfo {
    pub uid: String,
    pub name: String,
    /// spec.priority; None when unset or not an integer.
    pub priority: Option<i64>,
    /// spec.passthrough as written; None when unset. Parsed by `build` so an
    /// invalid value fails the catalog instead of silently caching.
    pub passthrough: Option<Value>,
    /// spec.nodeSelector as written; None when unset. Parsed by `build` so an
    /// invalid selector drops only this cache.
    pub node_selector: Option<Value>,
}

impl CacheInfo {
    pub fn from_json(v: &Value) -> Self {
        Self {
            uid: kube::str_at(v, &["metadata", "uid"]).into(),
            name: kube::str_at(v, &["metadata", "name"]).into(),
            priority: v.pointer("/spec/priority").and_then(Value::as_i64),
            passthrough: v
                .pointer("/spec/passthrough")
                .filter(|p| !p.is_null())
                .cloned(),
            node_selector: v
                .pointer("/spec/nodeSelector")
                .filter(|p| !p.is_null())
                .cloned(),
        }
    }
}

/// Cache definitions sorted by UID, with their nonempty node selectors.
#[derive(Clone, Debug, Default)]
pub struct Catalog {
    pub caches: Vec<CacheDefinition>,
    /// Caches without an entry use the default pool.
    pub selectors: BTreeMap<CacheId, Selector>,
    /// Names of caches left out for an invalid node selector.
    pub rejected: Vec<String>,
}

/// Map spec.passthrough to a cache mode. Absence caches; an object without a
/// route uses the CRD default, Peers. Anything else is invalid.
fn mode(passthrough: Option<&Value>) -> Result<wire::CacheMode, wire::Error> {
    let Some(passthrough) = passthrough else {
        return Ok(wire::CacheMode::Cached);
    };
    let fields = passthrough.as_object().ok_or(wire::Error::InvalidRequest)?;
    match fields.get("route") {
        None | Some(Value::Null) => Ok(wire::CacheMode::PassthroughPeers),
        Some(Value::String(route)) if route == "Peers" => Ok(wire::CacheMode::PassthroughPeers),
        Some(Value::String(route)) if route == "Local" => Ok(wire::CacheMode::PassthroughLocal),
        Some(_) => Err(wire::Error::InvalidRequest),
    }
}

/// Build the catalog sorted by UID. A cache with an invalid node selector is
/// left out and named in [`Catalog::rejected`]; any other invalid cache fails
/// the whole catalog.
pub fn build(caches: &[CacheInfo]) -> Result<Catalog, wire::Error> {
    let (mut ids, mut names) = (HashSet::new(), HashSet::new());
    let mut out = Vec::with_capacity(caches.len());
    let mut selectors = BTreeMap::new();
    let mut rejected = Vec::new();
    for c in caches {
        if !wire::valid_uuid(&c.uid) || !ids.insert(&c.uid) || !names.insert(&c.name) {
            return Err(wire::Error::InvalidRequest);
        }
        let selector = match Selector::parse(c.node_selector.as_ref()) {
            Ok(s) => s,
            Err(_) => {
                rejected.push(c.name.clone());
                continue;
            }
        };
        let (client, origin) = wire::canonical_socket_paths(&c.name)?;
        // The CRD bounds spec.priority; recheck so a skewed schema cannot publish
        // a catalog that every subscriber would reject.
        let priority = c.priority.unwrap_or(i64::from(wire::DEFAULT_PRIORITY));
        if !(0..=i64::from(wire::MAX_PRIORITY)).contains(&priority) {
            return Err(wire::Error::InvalidRequest);
        }
        out.push(CacheDefinition {
            id: CacheId(c.uid.clone()),
            name: c.name.clone(),
            client_socket: client,
            origin_socket: origin,
            priority: priority as u16,
            mode: mode(c.passthrough.as_ref())?,
        });
        if let Some(s) = selector {
            selectors.insert(CacheId(c.uid.clone()), s);
        }
    }
    out.sort_by(|a, b| a.id.cmp(&b.id));
    rejected.sort();
    Ok(Catalog {
        caches: out,
        selectors,
        rejected,
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    fn c(uid: &str, name: &str) -> CacheInfo {
        CacheInfo {
            uid: uid.into(),
            name: name.into(),
            priority: None,
            passthrough: None,
            node_selector: None,
        }
    }

    #[test]
    fn sorted_by_uid() {
        let out = build(&[
            c("22222222-2222-4222-8222-222222222222", "b"),
            c("11111111-1111-4111-8111-111111111111", "a"),
        ])
        .unwrap()
        .caches;
        assert_eq!(out[0].name, "a");
        assert_eq!(out[1].name, "b");
        let info = CacheInfo::from_json(&serde_json::json!({"metadata":{"uid":"u","name":"n"}}));
        assert_eq!((info.uid.as_str(), info.name.as_str()), ("u", "n"));
        assert_eq!(info.priority, None);
    }

    #[test]
    fn mixed_priorities_sorted_by_uid() {
        let with = |uid: &str, name: &str, priority: Option<i64>| CacheInfo {
            priority,
            ..c(uid, name)
        };
        let out = build(&[
            with("33333333-3333-4333-8333-333333333333", "max", Some(1000)),
            with("11111111-1111-4111-8111-111111111111", "min", Some(0)),
            with("22222222-2222-4222-8222-222222222222", "unset", None),
        ])
        .unwrap()
        .caches;
        let got: Vec<_> = out.iter().map(|d| (d.name.as_str(), d.priority)).collect();
        assert_eq!(
            got,
            [
                ("min", 0),
                ("unset", wire::DEFAULT_PRIORITY),
                ("max", wire::MAX_PRIORITY)
            ]
        );
    }

    #[test]
    fn priority_defaults_and_bounds() {
        let u = "11111111-1111-4111-8111-111111111111";
        let info = CacheInfo::from_json(
            &serde_json::json!({"metadata":{"uid":u,"name":"a"},"spec":{"priority":0}}),
        );
        assert_eq!(info.priority, Some(0));
        assert_eq!(build(&[info]).unwrap().caches[0].priority, 0);
        assert_eq!(
            build(&[c(u, "a")]).unwrap().caches[0].priority,
            wire::DEFAULT_PRIORITY
        );
        for (p, ok) in [(1000, true), (1001, false), (-1, false)] {
            let mut x = c(u, "a");
            x.priority = Some(p);
            assert_eq!(build(&[x]).is_ok(), ok, "priority {p}");
        }
    }

    #[test]
    fn passthrough_route_maps_to_mode() {
        let u = "11111111-1111-4111-8111-111111111111";
        let parse = |spec: Value| {
            let info = CacheInfo::from_json(
                &serde_json::json!({"metadata":{"uid":u,"name":"a"},"spec":spec}),
            );
            build(&[info]).map(|out| out.caches[0].mode)
        };
        assert_eq!(parse(serde_json::json!({})), Ok(wire::CacheMode::Cached));
        assert_eq!(
            parse(serde_json::json!({"passthrough":null})),
            Ok(wire::CacheMode::Cached)
        );
        assert_eq!(
            build(&[c(u, "a")]).unwrap().caches[0].mode,
            wire::CacheMode::Cached
        );
        for (spec, mode) in [
            (serde_json::json!({}), wire::CacheMode::PassthroughPeers),
            (
                serde_json::json!({"route":null}),
                wire::CacheMode::PassthroughPeers,
            ),
            (
                serde_json::json!({"route":"Peers"}),
                wire::CacheMode::PassthroughPeers,
            ),
            (
                serde_json::json!({"route":"Local"}),
                wire::CacheMode::PassthroughLocal,
            ),
        ] {
            assert_eq!(
                parse(serde_json::json!({"passthrough":spec})),
                Ok(mode),
                "{spec}"
            );
        }
        for bad in [
            serde_json::json!({"route":"peers"}),
            serde_json::json!({"route":"local"}),
            serde_json::json!({"route":""}),
            serde_json::json!({"route":"Mesh"}),
            serde_json::json!({"route":1}),
            serde_json::json!(true),
            serde_json::json!("Peers"),
            serde_json::json!([]),
        ] {
            assert!(
                parse(serde_json::json!({"passthrough":bad.clone()})).is_err(),
                "{bad}"
            );
        }
    }

    #[test]
    fn invalid_selector_drops_only_that_cache() {
        let info = |uid: &str, name: &str, selector: Value| {
            CacheInfo::from_json(&serde_json::json!({
                "metadata":{"uid":uid,"name":name},
                "spec":{"nodeSelector":selector}
            }))
        };
        let out = build(&[
            info(
                "11111111-1111-4111-8111-111111111111",
                "bad",
                serde_json::json!({"matchExpressions":[{"key":"gpu","operator":"In"}]}),
            ),
            info(
                "22222222-2222-4222-8222-222222222222",
                "gpu",
                serde_json::json!({"matchLabels":{"gpu":"yes"}}),
            ),
            info(
                "33333333-3333-4333-8333-333333333333",
                "plain",
                serde_json::json!({}),
            ),
            c("44444444-4444-4444-8444-444444444444", "unset"),
        ])
        .unwrap();
        let names: Vec<_> = out.caches.iter().map(|d| d.name.as_str()).collect();
        assert_eq!(names, ["gpu", "plain", "unset"]);
        assert_eq!(out.rejected, ["bad"]);
        assert_eq!(out.selectors.len(), 1);
        assert!(
            out.selectors
                .contains_key(&CacheId("22222222-2222-4222-8222-222222222222".into()))
        );
        // A rejected cache still reserves its UID and name.
        assert!(
            build(&[
                info(
                    "11111111-1111-4111-8111-111111111111",
                    "a",
                    serde_json::json!([])
                ),
                c("11111111-1111-4111-8111-111111111111", "b"),
            ])
            .is_err()
        );
    }

    #[test]
    fn rejects_invalid() {
        let u = "11111111-1111-4111-8111-111111111111";
        assert!(build(&[c("bad", "a")]).is_err());
        assert!(build(&[c(u, "a"), c(u, "b")]).is_err());
        assert!(build(&[c(u, "a"), c("22222222-2222-4222-8222-222222222222", "a")]).is_err());
        assert!(build(&[c(u, "A/B")]).is_err());
    }
}
