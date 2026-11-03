//! Cache node pools from `spec.nodeSelector`.
//!
//! A selector is a `metav1.LabelSelector` evaluated against Node labels with
//! Kubernetes semantics. Caches with the same canonical selector share a pool,
//! named `sel-` and the first 32 hex digits of SHA-256 of the canonical JSON.
//! Caches without a selector, or with an empty one, use the `default` pool of
//! every member.

use racer_control_wire::{CacheDefinition, CacheId, Pool, PoolId, Publication};
use serde::Serialize;
use serde_json::Value;
use sha2::{Digest, Sha256};
use std::collections::{BTreeMap, BTreeSet};

/// Why a selector is rejected. Only that cache is left out of the catalog.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct InvalidSelector;

/// A label selector operator.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Operator {
    In,
    NotIn,
    Exists,
    DoesNotExist,
}

impl Operator {
    /// Kubernetes spelling, also used for canonical ordering.
    pub fn as_str(self) -> &'static str {
        match self {
            Self::In => "In",
            Self::NotIn => "NotIn",
            Self::Exists => "Exists",
            Self::DoesNotExist => "DoesNotExist",
        }
    }
}

/// One `matchExpressions` entry with sorted, deduplicated values.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct Requirement {
    pub key: String,
    pub operator: Operator,
    pub values: Vec<String>,
}

impl Requirement {
    /// Canonical order: key, then operator spelling, then values.
    fn sort_key(&self) -> (&str, &str, &[String]) {
        (&self.key, self.operator.as_str(), &self.values)
    }

    /// Kubernetes semantics: `NotIn` and `DoesNotExist` match absent labels.
    fn matches(&self, labels: &BTreeMap<String, String>) -> bool {
        let value = labels.get(&self.key);
        match self.operator {
            Operator::In => value.is_some_and(|v| self.values.contains(v)),
            Operator::NotIn => value.is_none_or(|v| !self.values.contains(v)),
            Operator::Exists => value.is_some(),
            Operator::DoesNotExist => value.is_none(),
        }
    }
}

/// A nonempty, validated, canonical label selector.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct Selector {
    pub match_labels: BTreeMap<String, String>,
    pub expressions: Vec<Requirement>,
}

impl Selector {
    /// Parse `spec.nodeSelector`. Absent, null, or empty selects the default
    /// pool and returns `None`. Validation follows Kubernetes: label keys and
    /// values must be valid, `In` and `NotIn` need values, and `Exists` and
    /// `DoesNotExist` take none. Unknown fields are rejected.
    pub fn parse(v: Option<&Value>) -> Result<Option<Self>, InvalidSelector> {
        let Some(v) = v.filter(|v| !v.is_null()) else {
            return Ok(None);
        };
        let fields = v.as_object().ok_or(InvalidSelector)?;
        let mut match_labels = BTreeMap::new();
        let mut expressions = Vec::new();
        for (name, field) in fields {
            match (name.as_str(), field) {
                (_, Value::Null) if name == "matchLabels" || name == "matchExpressions" => {}
                ("matchLabels", Value::Object(labels)) => {
                    for (key, value) in labels {
                        let value = value.as_str().ok_or(InvalidSelector)?;
                        if !valid_key(key) || !valid_value(value) {
                            return Err(InvalidSelector);
                        }
                        match_labels.insert(key.clone(), value.to_owned());
                    }
                }
                ("matchExpressions", Value::Array(items)) => {
                    for item in items {
                        expressions.push(requirement(item)?);
                    }
                }
                _ => return Err(InvalidSelector),
            }
        }
        expressions.sort_by(|a, b| a.sort_key().cmp(&b.sort_key()));
        expressions.dedup();
        if match_labels.is_empty() && expressions.is_empty() {
            return Ok(None);
        }
        Ok(Some(Self {
            match_labels,
            expressions,
        }))
    }

    /// Whether Node labels satisfy every label and expression.
    pub fn matches(&self, labels: &BTreeMap<String, String>) -> bool {
        self.match_labels
            .iter()
            .all(|(k, v)| labels.get(k) == Some(v))
            && self.expressions.iter().all(|e| e.matches(labels))
    }

    /// Canonical JSON: sorted `matchLabels` keys, expressions sorted by key,
    /// operator, and values, each value list sorted and deduplicated, and
    /// empty fields left out.
    pub fn canonical_json(&self) -> String {
        #[derive(Serialize)]
        #[serde(rename_all = "camelCase")]
        struct Canonical<'a> {
            #[serde(skip_serializing_if = "BTreeMap::is_empty")]
            match_labels: &'a BTreeMap<String, String>,
            #[serde(skip_serializing_if = "Vec::is_empty")]
            match_expressions: Vec<Expression<'a>>,
        }
        #[derive(Serialize)]
        struct Expression<'a> {
            key: &'a str,
            operator: &'a str,
            #[serde(skip_serializing_if = "<[String]>::is_empty")]
            values: &'a [String],
        }
        serde_json::to_string(&Canonical {
            match_labels: &self.match_labels,
            match_expressions: self
                .expressions
                .iter()
                .map(|e| Expression {
                    key: &e.key,
                    operator: e.operator.as_str(),
                    values: &e.values,
                })
                .collect(),
        })
        .expect("canonical selector serializes")
    }

    /// `sel-` and the first 32 hex digits of SHA-256 of the canonical JSON.
    pub fn pool_id(&self) -> PoolId {
        let digest = Sha256::digest(self.canonical_json().as_bytes());
        let hex: String = digest[..16].iter().map(|b| format!("{b:02x}")).collect();
        PoolId(format!("sel-{hex}"))
    }
}

/// Parse one `matchExpressions` entry.
fn requirement(v: &Value) -> Result<Requirement, InvalidSelector> {
    let fields = v.as_object().ok_or(InvalidSelector)?;
    if fields
        .keys()
        .any(|k| !matches!(k.as_str(), "key" | "operator" | "values"))
    {
        return Err(InvalidSelector);
    }
    let key = fields
        .get("key")
        .and_then(Value::as_str)
        .filter(|k| valid_key(k))
        .ok_or(InvalidSelector)?;
    let operator = match fields.get("operator").and_then(Value::as_str) {
        Some("In") => Operator::In,
        Some("NotIn") => Operator::NotIn,
        Some("Exists") => Operator::Exists,
        Some("DoesNotExist") => Operator::DoesNotExist,
        _ => return Err(InvalidSelector),
    };
    let mut values = match fields.get("values") {
        None | Some(Value::Null) => Vec::new(),
        Some(Value::Array(items)) => items
            .iter()
            .map(|i| {
                i.as_str()
                    .filter(|s| valid_value(s))
                    .map(str::to_owned)
                    .ok_or(InvalidSelector)
            })
            .collect::<Result<_, _>>()?,
        Some(_) => return Err(InvalidSelector),
    };
    let needs_values = matches!(operator, Operator::In | Operator::NotIn);
    if needs_values == values.is_empty() {
        return Err(InvalidSelector);
    }
    values.sort();
    values.dedup();
    Ok(Requirement {
        key: key.to_owned(),
        operator,
        values,
    })
}

/// A label name: at most 63 characters, alphanumeric at both ends, and
/// `-`, `_`, or `.` between.
fn valid_name(s: &str) -> bool {
    let b = s.as_bytes();
    !b.is_empty()
        && b.len() <= 63
        && b[0].is_ascii_alphanumeric()
        && b[b.len() - 1].is_ascii_alphanumeric()
        && b.iter()
            .all(|c| c.is_ascii_alphanumeric() || matches!(c, b'-' | b'_' | b'.'))
}

/// A label value: empty or a label name.
fn valid_value(s: &str) -> bool {
    s.is_empty() || valid_name(s)
}

/// A label key: a name with an optional DNS-subdomain prefix and `/`.
fn valid_key(s: &str) -> bool {
    match s.split_once('/') {
        None => valid_name(s),
        Some((prefix, name)) => {
            valid_name(name)
                && !prefix.is_empty()
                && prefix.len() <= 253
                && prefix.split('.').all(|label| {
                    let b = label.as_bytes();
                    !b.is_empty()
                        && b.len() <= 63
                        && b[0].is_ascii_alphanumeric()
                        && b[b.len() - 1].is_ascii_alphanumeric()
                        && b.iter()
                            .all(|c| c.is_ascii_lowercase() || c.is_ascii_digit() || *c == b'-')
                })
        }
    }
}

/// A pool before member indexes are assigned. `nodes` holds Node UIDs, or
/// `None` for the default pool of every member.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct PoolPlan {
    pub id: PoolId,
    pub rank: u64,
    pub nodes: Option<BTreeSet<String>>,
    pub caches: Vec<CacheId>,
}

/// Group `caches` into pools by selector and evaluate each selector against
/// the labels of `members`. `selectors` maps cache IDs to nonempty selectors;
/// other caches use the default pool. Only pools some cache uses are returned,
/// sorted by ID.
pub fn plan<'a>(
    caches: &[CacheDefinition],
    selectors: &BTreeMap<CacheId, Selector>,
    members: impl IntoIterator<Item = (&'a str, &'a BTreeMap<String, String>)>,
) -> Vec<PoolPlan> {
    let members: Vec<_> = members.into_iter().collect();
    let mut pools: BTreeMap<PoolId, PoolPlan> = BTreeMap::new();
    for cache in caches {
        let selector = selectors.get(&cache.id);
        let id = selector.map_or_else(PoolId::default_pool, Selector::pool_id);
        let pool = pools.entry(id.clone()).or_insert_with(|| PoolPlan {
            id,
            rank: u64::from(selector.is_some()) << 32,
            nodes: selector.map(|s| {
                members
                    .iter()
                    .filter(|(_, labels)| s.matches(labels))
                    .map(|(node, _)| (*node).to_owned())
                    .collect()
            }),
            caches: Vec::new(),
        });
        pool.rank = pool
            .rank
            .max((pool.rank & !0xffff_ffff) | u64::from(cache.priority));
        pool.caches.push(cache.id.clone());
    }
    for pool in pools.values_mut() {
        pool.caches.sort();
    }
    pools.into_values().collect()
}

/// Pools of an earlier publication, with member indexes turned back into
/// Node UIDs. Used when the catalog is unknown and selectors cannot be
/// evaluated again.
pub fn from_publication(p: &Publication) -> Vec<PoolPlan> {
    let mut sorted: Vec<&str> = p.members.iter().map(|m| m.node.0.as_str()).collect();
    sorted.sort_unstable();
    p.pools
        .iter()
        .map(|pool| PoolPlan {
            id: pool.id.clone(),
            rank: pool.rank,
            nodes: pool.members.as_ref().map(|m| {
                m.iter()
                    .filter_map(|i| sorted.get(*i as usize).map(|n| (*n).to_owned()))
                    .collect()
            }),
            caches: pool.caches.clone(),
        })
        .collect()
}

/// Wire pools with indexes into `sorted_nodes`, the members sorted by Node
/// UID. Nodes that are not members are left out.
pub fn resolve(plans: &[PoolPlan], sorted_nodes: &[&str]) -> Vec<Pool> {
    plans
        .iter()
        .map(|p| Pool {
            id: p.id.clone(),
            rank: p.rank,
            members: p.nodes.as_ref().map(|nodes| {
                sorted_nodes
                    .iter()
                    .enumerate()
                    .filter(|(_, n)| nodes.contains(**n))
                    .map(|(i, _)| i as u32)
                    .collect()
            }),
            caches: p.caches.clone(),
        })
        .collect()
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    fn sel(v: Value) -> Selector {
        Selector::parse(Some(&v)).unwrap().unwrap()
    }

    fn labels(pairs: &[(&str, &str)]) -> BTreeMap<String, String> {
        pairs
            .iter()
            .map(|(k, v)| ((*k).into(), (*v).into()))
            .collect()
    }

    fn cache(n: u8, priority: u16) -> CacheDefinition {
        let name = format!("c{n}");
        let (client_socket, origin_socket) =
            racer_control_wire::canonical_socket_paths(&name).unwrap();
        CacheDefinition {
            id: CacheId(format!("{n:08x}-0000-4000-8000-000000000000")),
            name,
            client_socket,
            origin_socket,
            priority,
            mode: racer_control_wire::CacheMode::Cached,
        }
    }

    #[test]
    fn empty_selectors_use_the_default_pool() {
        for v in [
            None,
            Some(Value::Null),
            Some(json!({})),
            Some(json!({"matchLabels":{}})),
            Some(json!({"matchLabels":null,"matchExpressions":[]})),
        ] {
            assert_eq!(Selector::parse(v.as_ref()), Ok(None), "{v:?}");
        }
    }

    #[test]
    fn kubernetes_semantics() {
        let gpu = labels(&[("gpu", "a100"), ("zone", "z1")]);
        let cpu = labels(&[("zone", "z2")]);
        let none = labels(&[]);
        for (s, want) in [
            (json!({"matchLabels":{"gpu":"a100"}}), [true, false, false]),
            (json!({"matchLabels":{"gpu":""}}), [false, false, false]),
            (
                json!({"matchExpressions":[{"key":"gpu","operator":"In","values":["a100","h100"]}]}),
                [true, false, false],
            ),
            (
                json!({"matchExpressions":[{"key":"gpu","operator":"NotIn","values":["a100"]}]}),
                [false, true, true],
            ),
            (
                json!({"matchExpressions":[{"key":"gpu","operator":"Exists"}]}),
                [true, false, false],
            ),
            (
                json!({"matchExpressions":[{"key":"gpu","operator":"DoesNotExist","values":[]}]}),
                [false, true, true],
            ),
            (
                json!({"matchLabels":{"zone":"z1"},"matchExpressions":[{"key":"gpu","operator":"Exists"}]}),
                [true, false, false],
            ),
            (
                json!({"matchLabels":{"zone":"z2"},"matchExpressions":[{"key":"gpu","operator":"Exists"}]}),
                [false, false, false],
            ),
        ] {
            let s = sel(s.clone());
            assert_eq!(
                [s.matches(&gpu), s.matches(&cpu), s.matches(&none)],
                want,
                "{}",
                s.canonical_json()
            );
        }
    }

    #[test]
    fn invalid_selectors() {
        for v in [
            json!([]),
            json!("gpu=a100"),
            json!({"matchLabels":[]}),
            json!({"matchLabels":{"gpu":1}}),
            json!({"matchLabels":{"-gpu":"a"}}),
            json!({"matchLabels":{"gpu":"a b"}}),
            json!({"matchLabels":{"Example.com/gpu":"a"}}),
            json!({"matchLabels":{"/gpu":"a"}}),
            json!({"matchLabels":{"gpu":"x".repeat(64)}}),
            json!({"future":{}}),
            json!({"matchExpressions":{}}),
            json!({"matchExpressions":[{"key":"gpu","operator":"In"}]}),
            json!({"matchExpressions":[{"key":"gpu","operator":"In","values":[]}]}),
            json!({"matchExpressions":[{"key":"gpu","operator":"NotIn","values":null}]}),
            json!({"matchExpressions":[{"key":"gpu","operator":"Exists","values":["a"]}]}),
            json!({"matchExpressions":[{"key":"gpu","operator":"DoesNotExist","values":["a"]}]}),
            json!({"matchExpressions":[{"key":"gpu","operator":"Gt","values":["1"]}]}),
            json!({"matchExpressions":[{"key":"gpu","operator":"in","values":["a"]}]}),
            json!({"matchExpressions":[{"operator":"Exists"}]}),
            json!({"matchExpressions":[{"key":"gpu"}]}),
            json!({"matchExpressions":[{"key":"gpu","operator":"In","values":[1]}]}),
            json!({"matchExpressions":[{"key":"gpu","operator":"Exists","x":1}]}),
            json!({"matchExpressions":["gpu"]}),
        ] {
            assert_eq!(Selector::parse(Some(&v)), Err(InvalidSelector), "{v}");
        }
        // Prefixed keys and empty values are valid.
        sel(json!({"matchLabels":{"example.com/gpu":""}}));
    }

    #[test]
    fn canonical_id_ignores_order_and_duplicates() {
        let a = sel(json!({
            "matchLabels":{"b":"2","a":"1"},
            "matchExpressions":[
                {"key":"z","operator":"Exists"},
                {"key":"g","operator":"NotIn","values":["y","x","x"]},
                {"key":"g","operator":"In","values":["b","a"]},
            ]
        }));
        let b = sel(json!({
            "matchExpressions":[
                {"key":"g","operator":"In","values":["a","b","a"]},
                {"values":["x","y"],"operator":"NotIn","key":"g"},
                {"key":"z","operator":"Exists"},
                {"key":"z","operator":"Exists","values":null},
            ],
            "matchLabels":{"a":"1","b":"2"}
        }));
        assert_eq!(a, b);
        assert_eq!(
            a.canonical_json(),
            r#"{"matchLabels":{"a":"1","b":"2"},"matchExpressions":[{"key":"g","operator":"In","values":["a","b"]},{"key":"g","operator":"NotIn","values":["x","y"]},{"key":"z","operator":"Exists"}]}"#
        );
        let id = a.pool_id();
        assert_eq!(id, b.pool_id());
        assert!(id.valid() && !id.is_default(), "{id:?}");
        let digest = Sha256::digest(a.canonical_json().as_bytes());
        assert_eq!(
            id.0,
            format!(
                "sel-{}",
                &digest
                    .iter()
                    .map(|b| format!("{b:02x}"))
                    .collect::<String>()[..32]
            )
        );
        assert_ne!(id, sel(json!({"matchLabels":{"a":"1"}})).pool_id());
        // matchLabels and an equivalent In expression are distinct selectors.
        assert_ne!(
            sel(json!({"matchLabels":{"a":"1"}})).pool_id(),
            sel(json!({"matchExpressions":[{"key":"a","operator":"In","values":["1"]}]})).pool_id()
        );
    }

    #[test]
    fn grouping_ranks_and_membership() {
        let gpu = sel(json!({"matchLabels":{"gpu":"yes"}}));
        let same = sel(json!({"matchLabels":{"gpu":"yes"}}));
        let zone = sel(json!({"matchExpressions":[{"key":"zone","operator":"Exists"}]}));
        let caches = [
            cache(1, 100),
            cache(2, 900),
            cache(3, 10),
            cache(4, 1000),
            cache(5, 0),
        ];
        let selectors = BTreeMap::from([
            (caches[0].id.clone(), gpu.clone()),
            (caches[1].id.clone(), same),
            (caches[4].id.clone(), zone.clone()),
        ]);
        let (l1, l2, l3) = (
            labels(&[("gpu", "yes")]),
            labels(&[("zone", "a")]),
            labels(&[]),
        );
        let members = [("n1", &l1), ("n2", &l2), ("n3", &l3)];
        let pools = plan(&caches, &selectors, members);
        let mut want = vec![
            PoolPlan {
                id: PoolId::default_pool(),
                rank: 1000,
                nodes: None,
                caches: vec![caches[2].id.clone(), caches[3].id.clone()],
            },
            PoolPlan {
                id: gpu.pool_id(),
                rank: (1 << 32) | 900,
                nodes: Some(BTreeSet::from(["n1".into()])),
                caches: vec![caches[0].id.clone(), caches[1].id.clone()],
            },
            PoolPlan {
                id: zone.pool_id(),
                rank: 1 << 32,
                nodes: Some(BTreeSet::from(["n2".into()])),
                caches: vec![caches[4].id.clone()],
            },
        ];
        want.sort_by(|a, b| a.id.cmp(&b.id));
        assert_eq!(pools, want);
        // Selector pools outrank the default pool regardless of priority.
        assert!(pools.iter().all(|p| p.id.is_default() || p.rank > 1000));
        // Only used pools: without selected caches, only the default pool.
        let only_default = plan(&caches[2..4], &selectors, members);
        assert_eq!(only_default.len(), 1);
        assert!(only_default[0].id.is_default());
        assert!(plan(&[], &selectors, members).is_empty());
        // Resolve to indexes into the sorted members; non-members are dropped.
        let wire = resolve(&pools, &["n0", "n1", "n2"]);
        let by_id = |id: &PoolId| wire.iter().find(|p| &p.id == id).unwrap();
        assert_eq!(by_id(&gpu.pool_id()).members, Some(vec![1]));
        assert_eq!(by_id(&zone.pool_id()).members, Some(vec![2]));
        assert_eq!(by_id(&PoolId::default_pool()).members, None);
        // A pool with no matching member is still published, empty.
        let empty = plan(&caches[..1], &selectors, [("n3", &l3)]);
        assert_eq!(empty[0].nodes, Some(BTreeSet::new()));
    }

    #[test]
    fn label_changes_move_nodes() {
        let gpu = sel(json!({"matchExpressions":[{"key":"gpu","operator":"In","values":["yes"]}]}));
        let caches = [cache(1, 5)];
        let selectors = BTreeMap::from([(caches[0].id.clone(), gpu)]);
        let (yes, no) = (labels(&[("gpu", "yes")]), labels(&[("gpu", "no")]));
        let before = plan(&caches, &selectors, [("n1", &yes), ("n2", &no)]);
        let after = plan(&caches, &selectors, [("n1", &no), ("n2", &yes)]);
        assert_eq!(before[0].nodes, Some(BTreeSet::from(["n1".into()])));
        assert_eq!(after[0].nodes, Some(BTreeSet::from(["n2".into()])));
        assert_eq!(before[0].id, after[0].id);
    }
}
