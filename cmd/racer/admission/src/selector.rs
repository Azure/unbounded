//! ClusterCache lookup and its `spec.nodeSelector` as node affinity.
//!
//! A cache with a selector serves only the nodes the selector matches, so a
//! pod bound to it must schedule there. The selector is a
//! `metav1.LabelSelector`. Each `matchLabels` entry becomes an `In`
//! requirement with one value, and each `matchExpressions` entry is copied.
//! The four LabelSelector operators have the same names and meaning in a
//! `NodeSelectorRequirement`.

use serde_json::{Map, Value, json};

use super::*;

/// Looks up `cache` and returns the node requirements its selector adds.
/// Empty when the cache has no selector or an empty one.
pub(crate) fn cache_requirements(
    cache: &str,
    annotation: &str,
    lookup: &dyn Fn(&str) -> CacheLookup,
) -> Result<Vec<Value>, Reject> {
    match lookup(cache) {
        CacheLookup::Found(None) => Ok(Vec::new()),
        CacheLookup::Found(Some(sel)) => requirements(&sel).map_err(|e| {
            deny(format!(
                "ClusterCache {cache:?} named by annotation {annotation} has an invalid spec.nodeSelector: {e}"
            ))
        }),
        CacheLookup::NotFound => Err(deny(format!(
            "annotation {annotation} names ClusterCache {cache:?}, which does not exist"
        ))),
        CacheLookup::Unavailable => Err(Reject::Unavailable(format!(
            "ClusterCache {cache:?} named by annotation {annotation} could not be looked up; retry"
        ))),
    }
}

/// Converts a LabelSelector to node selector requirements, validating it the
/// way the Kubernetes API server does.
pub(crate) fn requirements(sel: &Value) -> Result<Vec<Value>, String> {
    let sel = match sel {
        Value::Null => return Ok(Vec::new()),
        Value::Object(o) => o,
        _ => return Err("must be an object".into()),
    };
    let mut out = Vec::new();
    match sel.get("matchLabels") {
        None | Some(Value::Null) => {}
        Some(Value::Object(labels)) => {
            let mut labels: Vec<_> = labels.iter().collect();
            labels.sort_by(|a, b| a.0.cmp(b.0));
            for (k, v) in labels {
                let v = v
                    .as_str()
                    .ok_or_else(|| format!("matchLabels[{k:?}] must be a string"))?;
                check_key(k)?;
                check_value(v)?;
                out.push(json!({"key": k, "operator": "In", "values": [v]}));
            }
        }
        Some(_) => return Err("matchLabels must be an object".into()),
    }
    match sel.get("matchExpressions") {
        None | Some(Value::Null) => {}
        Some(Value::Array(exprs)) => {
            for (i, e) in exprs.iter().enumerate() {
                out.push(expression(e).map_err(|m| format!("matchExpressions[{i}]: {m}"))?);
            }
        }
        Some(_) => return Err("matchExpressions must be an array".into()),
    }
    Ok(out)
}

fn expression(e: &Value) -> Result<Value, String> {
    let e: &Map<String, Value> = e.as_object().ok_or("must be an object")?;
    let key = e
        .get("key")
        .and_then(Value::as_str)
        .ok_or("key must be a string")?;
    check_key(key)?;
    let op = e
        .get("operator")
        .and_then(Value::as_str)
        .ok_or("operator must be a string")?;
    let values = match e.get("values") {
        None | Some(Value::Null) => Vec::new(),
        Some(Value::Array(v)) => v
            .iter()
            .map(|v| v.as_str().ok_or("values must be strings"))
            .collect::<Result<Vec<_>, _>>()?,
        Some(_) => return Err("values must be an array".into()),
    };
    match op {
        "In" | "NotIn" => {
            if values.is_empty() {
                return Err(format!("operator {op} requires at least one value"));
            }
            for v in &values {
                check_value(v)?;
            }
            Ok(json!({"key": key, "operator": op, "values": values}))
        }
        "Exists" | "DoesNotExist" => {
            if !values.is_empty() {
                return Err(format!("operator {op} must not have values"));
            }
            Ok(json!({"key": key, "operator": op}))
        }
        _ => Err(format!(
            "operator {op:?} is not one of In, NotIn, Exists, DoesNotExist"
        )),
    }
}

/// A DNS-1123 subdomain prefix, then a name, as in Kubernetes label keys.
fn check_key(key: &str) -> Result<(), String> {
    let (prefix, name) = match key.rsplit_once('/') {
        Some((p, n)) => (Some(p), n),
        None => (None, key),
    };
    if let Some(p) = prefix {
        let ok = !p.is_empty()
            && p.len() <= 253
            && p.split('.').all(|l| {
                !l.is_empty()
                    && l.len() <= 63
                    && !l.starts_with('-')
                    && !l.ends_with('-')
                    && l.bytes()
                        .all(|c| c.is_ascii_lowercase() || c.is_ascii_digit() || c == b'-')
            });
        if !ok {
            return Err(format!("label key {key:?} has an invalid prefix"));
        }
    }
    if name.is_empty() || !label_name(name) {
        return Err(format!("label key {key:?} is invalid"));
    }
    Ok(())
}

fn check_value(v: &str) -> Result<(), String> {
    if v.is_empty() || label_name(v) {
        Ok(())
    } else {
        Err(format!("label value {v:?} is invalid"))
    }
}

/// At most 63 alphanumerics, `-`, `_`, or `.`, starting and ending alphanumeric.
fn label_name(s: &str) -> bool {
    let b = s.as_bytes();
    !b.is_empty()
        && b.len() <= 63
        && b[0].is_ascii_alphanumeric()
        && b[b.len() - 1].is_ascii_alphanumeric()
        && b.iter()
            .all(|c| c.is_ascii_alphanumeric() || matches!(c, b'-' | b'_' | b'.'))
}
