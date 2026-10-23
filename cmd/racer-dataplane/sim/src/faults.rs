//! Fault schedule: partitions, crash/restart, slow nodes, origin errors.
//!
//! Faults are data. A scenario turns them into events at start-up and asks
//! [`Partitions`] whether two nodes can talk at a given time.

use serde_json::Value;

use crate::config::{field, spec};
use crate::engine::Nanos;
use crate::resources::OriginErrors;

#[derive(Clone, Debug, PartialEq)]
pub enum Fault {
    /// Sites `a` and `b` cannot reach each other during `[from_ns, until_ns)`.
    Partition {
        from_ns: Nanos,
        until_ns: Nanos,
        site_a: u16,
        site_b: u16,
    },
    /// Node loses memory state at `at_ns` and comes back at `restart_ns`.
    /// Its disk survives.
    Crash {
        node: u32,
        at_ns: Nanos,
        restart_ns: Nanos,
    },
    /// Node runs `factor` times slower during `[from_ns, until_ns)`.
    Slow {
        node: u32,
        factor: u8,
        from_ns: Nanos,
        until_ns: Nanos,
    },
    /// Replace origin error rates during `[from_ns, until_ns)`.
    OriginErrors {
        from_ns: Nanos,
        until_ns: Nanos,
        errors: OriginErrors,
    },
}

fn small<T: TryFrom<u64>>(o: &serde_json::Map<String, Value>, k: &str) -> Result<T, String> {
    T::try_from(field(o, k)?).map_err(|_| format!("{k} out of range"))
}

fn prob(o: &serde_json::Map<String, Value>, k: &str) -> Result<f64, String> {
    match o.get(k) {
        None => Ok(0.0),
        Some(v) => v
            .as_f64()
            .filter(|p| (0.0..=1.0).contains(p))
            .ok_or_else(|| format!("{k} must be a probability")),
    }
}

impl Fault {
    pub fn from_json(v: &Value) -> Result<Self, String> {
        let kind = v
            .get("kind")
            .and_then(Value::as_str)
            .ok_or("missing string field \"kind\"")?;
        let fault = match kind {
            "partition" => {
                let o = spec(v, &["kind", "from_ns", "until_ns", "site_a", "site_b"])?;
                Self::Partition {
                    from_ns: field(o, "from_ns")?,
                    until_ns: field(o, "until_ns")?,
                    site_a: small(o, "site_a")?,
                    site_b: small(o, "site_b")?,
                }
            }
            "crash" => {
                let o = spec(v, &["kind", "node", "at_ns", "restart_ns"])?;
                Self::Crash {
                    node: small(o, "node")?,
                    at_ns: field(o, "at_ns")?,
                    restart_ns: field(o, "restart_ns")?,
                }
            }
            "slow" => {
                let o = spec(v, &["kind", "node", "factor", "from_ns", "until_ns"])?;
                Self::Slow {
                    node: small(o, "node")?,
                    factor: small::<u8>(o, "factor")?.max(1),
                    from_ns: field(o, "from_ns")?,
                    until_ns: field(o, "until_ns")?,
                }
            }
            "origin_errors" => {
                let o = spec(v, &["kind", "from_ns", "until_ns", "p412", "p403", "p5xx"])?;
                let errors = OriginErrors {
                    p412: prob(o, "p412")?,
                    p403: prob(o, "p403")?,
                    p5xx: prob(o, "p5xx")?,
                };
                if errors.p412 + errors.p403 + errors.p5xx > 1.0 {
                    return Err("origin error probabilities sum above 1".into());
                }
                Self::OriginErrors {
                    from_ns: field(o, "from_ns")?,
                    until_ns: field(o, "until_ns")?,
                    errors,
                }
            }
            other => return Err(format!("unknown fault kind {other:?}")),
        };
        Ok(fault)
    }
}

/// Active partitions, queried per message.
#[derive(Clone, Debug, Default)]
pub struct Partitions {
    cuts: Vec<(Nanos, Nanos, u16, u16)>,
}

impl Partitions {
    pub fn from_faults(faults: &[Fault]) -> Self {
        let cuts = faults
            .iter()
            .filter_map(|f| match *f {
                Fault::Partition {
                    from_ns,
                    until_ns,
                    site_a,
                    site_b,
                } => Some((from_ns, until_ns, site_a, site_b)),
                _ => None,
            })
            .collect();
        Self { cuts }
    }

    pub fn blocked(&self, now: Nanos, a: u16, b: u16) -> bool {
        a != b
            && self.cuts.iter().any(|&(from, until, x, y)| {
                (from..until).contains(&now) && ((x, y) == (a, b) || (x, y) == (b, a))
            })
    }
}

/// Origin error rates in effect at `now`: the last matching window wins,
/// otherwise `base`.
pub fn origin_errors_at(faults: &[Fault], base: OriginErrors, now: Nanos) -> OriginErrors {
    faults
        .iter()
        .rev()
        .find_map(|f| match *f {
            Fault::OriginErrors {
                from_ns,
                until_ns,
                errors,
            } if (from_ns..until_ns).contains(&now) => Some(errors),
            _ => None,
        })
        .unwrap_or(base)
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn parses_every_kind() {
        for v in [
            json!({"kind": "partition", "from_ns": 1, "until_ns": 2, "site_a": 0, "site_b": 1}),
            json!({"kind": "crash", "node": 1, "at_ns": 1, "restart_ns": 2}),
            json!({"kind": "slow", "node": 1, "factor": 0, "from_ns": 1, "until_ns": 2}),
            json!({"kind": "origin_errors", "from_ns": 1, "until_ns": 2, "p5xx": 0.5}),
        ] {
            Fault::from_json(&v).unwrap();
        }
        assert_eq!(
            Fault::from_json(
                &json!({"kind": "slow", "node": 1, "factor": 0, "from_ns": 1, "until_ns": 2})
            )
            .unwrap(),
            Fault::Slow {
                node: 1,
                factor: 1,
                from_ns: 1,
                until_ns: 2
            }
        );
        for bad in [
            json!({}),
            json!({"kind": "crash", "node": 1, "at_ns": 1, "restart_ns": 2, "x": 1}),
            json!({"kind": "partition", "from_ns": 1, "until_ns": 2, "site_a": 70000, "site_b": 1}),
            json!({"kind": "origin_errors", "from_ns": 1, "until_ns": 2, "p5xx": 1.5}),
            json!({"kind": "origin_errors", "from_ns": 1, "until_ns": 2, "p5xx": 0.6, "p403": 0.6}),
        ] {
            assert!(Fault::from_json(&bad).is_err(), "{bad}");
        }
    }

    #[test]
    fn partitions_are_windowed_and_symmetric() {
        let p = Partitions::from_faults(&[Fault::Partition {
            from_ns: 10,
            until_ns: 20,
            site_a: 1,
            site_b: 2,
        }]);
        assert!(p.blocked(10, 1, 2));
        assert!(p.blocked(19, 2, 1));
        assert!(!p.blocked(20, 1, 2));
        assert!(!p.blocked(15, 1, 3));
        assert!(!p.blocked(15, 1, 1));
    }

    #[test]
    fn origin_error_windows() {
        let base = OriginErrors::default();
        let hot = OriginErrors { p5xx: 1.0, ..base };
        let f = [Fault::OriginErrors {
            from_ns: 5,
            until_ns: 10,
            errors: hot,
        }];
        assert_eq!(origin_errors_at(&f, base, 4), base);
        assert_eq!(origin_errors_at(&f, base, 5), hot);
        assert_eq!(origin_errors_at(&f, base, 10), base);
    }
}
