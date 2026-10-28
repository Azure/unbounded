//! Lease JSON conversion and writes.

use crate::{
    authority::election::{LeaseObservation, LeaseSpec},
    kube::{Kube, KubeError, map_at, paths, str_at},
};
use serde_json::{Value, json};
use std::collections::BTreeMap;

/// Parse a Lease object.
pub fn observe(v: &Value) -> LeaseObservation {
    let spec = &v["spec"];
    LeaseObservation {
        name: str_at(v, &["metadata", "name"]).into(),
        uid: str_at(v, &["metadata", "uid"]).into(),
        resource_version: str_at(v, &["metadata", "resourceVersion"]).into(),
        spec: LeaseSpec {
            holder: spec["holderIdentity"]
                .as_str()
                .filter(|s| !s.is_empty())
                .map(Into::into),
            duration_secs: spec["leaseDurationSeconds"].as_u64().unwrap_or(0).min(3600) as u32,
            acquire_time: spec["acquireTime"].as_str().map(Into::into),
            renew_time: spec["renewTime"].as_str().map(Into::into),
            transitions: spec["leaseTransitions"].as_u64().unwrap_or(0),
        },
        annotations: map_at(v, &["metadata", "annotations"]),
    }
}

/// Holder key `<pod name>/<nonce>`, or `None` when unheld.
pub fn holder_key(o: &LeaseObservation) -> Option<String> {
    let holder = o.spec.holder.as_deref()?;
    let nonce = o
        .annotations
        .get(crate::annotations::HOLDER_NONCE)
        .map_or("", String::as_str);
    Some(format!("{holder}/{nonce}"))
}

/// Replace spec and the Racer annotations of `object`, keeping other fields.
/// `annotations` values of `None` are removed.
pub fn with(
    object: &Value,
    spec: &LeaseSpec,
    annotations: &BTreeMap<&str, Option<String>>,
) -> Value {
    let mut v = object.clone();
    v["spec"] = json!({
        "holderIdentity": spec.holder,
        "leaseDurationSeconds": spec.duration_secs,
        "acquireTime": spec.acquire_time,
        "renewTime": spec.renew_time,
        "leaseTransitions": spec.transitions,
    });
    if !v["metadata"]["annotations"].is_object() {
        v["metadata"]["annotations"] = json!({});
    }
    let a = v["metadata"]["annotations"]
        .as_object_mut()
        .expect("object");
    for (k, val) in annotations {
        match val {
            Some(s) => {
                a.insert((*k).into(), s.clone().into());
            }
            None => {
                a.remove(*k);
            }
        }
    }
    v
}

/// GET a named Lease.
pub async fn get(api: &Kube, ns: &str, name: &str) -> Result<Value, KubeError> {
    api.get(&paths::lease(ns, name)).await
}

/// CAS PUT of a Lease.
pub async fn put(api: &Kube, ns: &str, object: &Value) -> Result<Value, KubeError> {
    let name = str_at(object, &["metadata", "name"]).to_owned();
    api.put(&paths::lease(ns, &name), object).await
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn roundtrip() {
        let v = json!({"metadata":{"name":"l","uid":"u","resourceVersion":"3","annotations":{"other":"x","racer.unbounded-cloud.io/ready":"1"}},
            "spec":{"holderIdentity":"p/0000000000000000","leaseDurationSeconds":30,"leaseTransitions":4}});
        let o = observe(&v);
        assert_eq!(o.spec.transitions, 4);
        assert_eq!(o.spec.holder.as_deref(), Some("p/0000000000000000"));
        let mut a = BTreeMap::new();
        a.insert(crate::annotations::READY, None);
        a.insert(crate::annotations::ENDPOINT, Some("https://x".into()));
        let w = with(
            &v,
            &LeaseSpec {
                transitions: 5,
                ..o.spec.clone()
            },
            &a,
        );
        let o2 = observe(&w);
        assert_eq!(o2.spec.transitions, 5);
        assert_eq!(o2.annotations.get("other").map(String::as_str), Some("x"));
        assert!(!o2.annotations.contains_key(crate::annotations::READY));
        assert_eq!(o2.resource_version, "3");
        assert!(observe(&json!({})).spec.holder.is_none());
    }
}
