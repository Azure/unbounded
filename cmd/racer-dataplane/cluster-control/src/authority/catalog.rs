//! Cache catalog from ClusterCaches, like members.BuildCatalog in the old controller.

use crate::kube;
use racer_control_wire::{self as wire, CacheDefinition, CacheId};
use serde_json::Value;
use std::collections::HashSet;

/// ClusterCache fields used here.
#[derive(Clone, Debug, Default)]
pub struct CacheInfo {
    pub uid: String,
    pub name: String,
    /// spec.priority; None when unset or not an integer.
    pub priority: Option<i64>,
}

impl CacheInfo {
    pub fn from_json(v: &Value) -> Self {
        Self {
            uid: kube::str_at(v, &["metadata", "uid"]).into(),
            name: kube::str_at(v, &["metadata", "name"]).into(),
            priority: v.pointer("/spec/priority").and_then(Value::as_i64),
        }
    }
}

/// Build the catalog sorted by UID. Any invalid cache fails the whole catalog.
pub fn build(caches: &[CacheInfo]) -> Result<Vec<CacheDefinition>, wire::Error> {
    let (mut ids, mut names) = (HashSet::new(), HashSet::new());
    let mut out = Vec::with_capacity(caches.len());
    for c in caches {
        if !wire::valid_uuid(&c.uid) || !ids.insert(&c.uid) || !names.insert(&c.name) {
            return Err(wire::Error::InvalidRequest);
        }
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
        });
    }
    out.sort_by(|a, b| a.id.cmp(&b.id));
    Ok(out)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn c(uid: &str, name: &str) -> CacheInfo {
        CacheInfo {
            uid: uid.into(),
            name: name.into(),
            priority: None,
        }
    }

    #[test]
    fn sorted_by_uid() {
        let out = build(&[
            c("22222222-2222-4222-8222-222222222222", "b"),
            c("11111111-1111-4111-8111-111111111111", "a"),
        ])
        .unwrap();
        assert_eq!(out[0].name, "a");
        assert_eq!(out[1].name, "b");
        let info = CacheInfo::from_json(&serde_json::json!({"metadata":{"uid":"u","name":"n"}}));
        assert_eq!((info.uid.as_str(), info.name.as_str()), ("u", "n"));
        assert_eq!(info.priority, None);
    }

    #[test]
    fn priority_defaults_and_bounds() {
        let u = "11111111-1111-4111-8111-111111111111";
        let info = CacheInfo::from_json(
            &serde_json::json!({"metadata":{"uid":u,"name":"a"},"spec":{"priority":0}}),
        );
        assert_eq!(info.priority, Some(0));
        assert_eq!(build(&[info]).unwrap()[0].priority, 0);
        assert_eq!(
            build(&[c(u, "a")]).unwrap()[0].priority,
            wire::DEFAULT_PRIORITY
        );
        for (p, ok) in [(1000, true), (1001, false), (-1, false)] {
            let mut x = c(u, "a");
            x.priority = Some(p);
            assert_eq!(build(&[x]).is_ok(), ok, "priority {p}");
        }
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
