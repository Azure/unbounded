//! Workload generators: Poisson arrivals, Zipf popularity, range reads,
//! cold-start storms and membership churn schedules.

use racer_core::time::SplitMix64;
use serde_json::Value;

use crate::config::{field, spec};
use crate::engine::{Nanos, SECONDS, unit};

/// Exponential inter-arrival times for a Poisson process.
#[derive(Clone, Copy, Debug)]
pub struct Poisson {
    mean_ns: f64,
}

impl Poisson {
    /// `rate` events per second. A zero rate never fires.
    pub fn new(rate: f64) -> Self {
        Self {
            mean_ns: if rate > 0.0 {
                SECONDS as f64 / rate
            } else {
                f64::INFINITY
            },
        }
    }

    /// Next gap, or `None` for a zero rate.
    pub fn gap(&self, rng: &mut SplitMix64) -> Option<Nanos> {
        if !self.mean_ns.is_finite() {
            return None;
        }
        let u = 1.0 - unit(rng); // (0, 1]
        Some(((-u.ln()) * self.mean_ns).ceil().max(1.0) as Nanos)
    }
}

/// Zipf sampler over ranks `0..n` with exponent `s`, by inverse CDF. Memory is `8 * n` bytes;
/// shared by all nodes.
#[derive(Clone, Debug)]
pub struct Zipf {
    cdf: Vec<f64>,
}

impl Zipf {
    pub fn new(n: u64, s: f64) -> Self {
        let n = n.max(1);
        let mut cdf = Vec::with_capacity(n as usize);
        let mut sum = 0.0;
        for k in 1..=n {
            sum += 1.0 / (k as f64).powf(s);
            cdf.push(sum);
        }
        for v in &mut cdf {
            *v /= sum;
        }
        Self { cdf }
    }

    pub fn sample(&self, rng: &mut SplitMix64) -> u64 {
        let u = unit(rng);
        (self.cdf.partition_point(|&c| c <= u) as u64).min(self.cdf.len() as u64 - 1)
    }

    pub fn approximate_bytes(&self) -> usize {
        self.cdf.capacity() * 8
    }
}

/// A read: `pages` consecutive pages of `object` starting at `first`.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct Read {
    pub object: u64,
    pub first: u32,
    pub pages: u32,
}

/// Draw reads: Zipf object, uniform page, optionally a range.
pub struct Reads {
    zipf: Zipf,
    pages_per_object: u32,
    range_fraction: f64,
    range_pages: u32,
}

impl Reads {
    pub fn new(
        objects: u64,
        s: f64,
        pages_per_object: u32,
        range_fraction: f64,
        range_pages: u32,
    ) -> Self {
        Self {
            zipf: Zipf::new(objects, s),
            pages_per_object: pages_per_object.max(1),
            range_fraction,
            range_pages: range_pages.max(1),
        }
    }

    pub fn draw(&self, rng: &mut SplitMix64) -> Read {
        let object = self.zipf.sample(rng);
        let first = rng.below(u64::from(self.pages_per_object)) as u32;
        let pages = if unit(rng) < self.range_fraction {
            self.range_pages.min(self.pages_per_object - first)
        } else {
            1
        };
        Read {
            object,
            first,
            pages,
        }
    }

    pub fn approximate_bytes(&self) -> usize {
        self.zipf.approximate_bytes()
    }
}

/// Global page id for `(object, page)`.
pub fn page_id(object: u64, page: u32, pages_per_object: u32) -> u64 {
    object * u64::from(pages_per_object) + u64::from(page)
}

/// Cold-start storm: `nodes` nodes (the first `nodes` live ids from a seeded shuffle) all read
/// every page of `object` within `window_ns` after `at_ns`.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct Storm {
    pub at_ns: Nanos,
    pub object: u64,
    pub nodes: u32,
    pub window_ns: Nanos,
}

impl Storm {
    pub fn from_json(v: &Value) -> Result<Self, String> {
        let o = spec(v, &["at_ns", "object", "nodes", "window_ns"])?;
        Ok(Self {
            at_ns: field(o, "at_ns")?,
            object: field(o, "object")?,
            nodes: u32::try_from(field(o, "nodes")?).map_err(|_| "nodes out of range")?,
            window_ns: field(o, "window_ns")?,
        })
    }

    /// Expand into `(time, node)` start events, using `rng` for the node choice and arrival jitter.
    /// Node ids come from `0..total`.
    pub fn expand(&self, total: u32, rng: &mut SplitMix64) -> Vec<(Nanos, u32)> {
        let count = self.nodes.min(total) as usize;
        // Partial Fisher-Yates on a lazily materialized permutation.
        let mut swaps: std::collections::HashMap<u32, u32> = Default::default();
        let mut out = Vec::with_capacity(count);
        for i in 0..count as u32 {
            let j = i + rng.below(u64::from(total - i)) as u32;
            let vi = *swaps.get(&i).unwrap_or(&i);
            let vj = *swaps.get(&j).unwrap_or(&j);
            swaps.insert(j, vi);
            let jitter = rng.below(self.window_ns.max(1));
            out.push((self.at_ns + jitter, vj));
        }
        out.sort_unstable();
        out
    }
}

/// Membership churn: `node` joins or leaves at `at_ns`.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct Churn {
    pub at_ns: Nanos,
    pub node: u32,
    pub join: bool,
}

impl Churn {
    pub fn from_json(v: &Value) -> Result<Self, String> {
        let o = spec(v, &["at_ns", "node", "join"])?;
        Ok(Self {
            at_ns: field(o, "at_ns")?,
            node: u32::try_from(field(o, "node")?).map_err(|_| "node out of range")?,
            join: o
                .get("join")
                .and_then(Value::as_bool)
                .ok_or("missing bool field \"join\"")?,
        })
    }

    /// Random churn: `events` leave/rejoin pairs spread over `duration`, each node away for
    /// `away_ns`.
    pub fn random(
        nodes: u32,
        events: u32,
        duration: Nanos,
        away_ns: Nanos,
        rng: &mut SplitMix64,
    ) -> Vec<Churn> {
        let mut out = Vec::with_capacity(events as usize * 2);
        for _ in 0..events {
            let node = rng.below(u64::from(nodes)) as u32;
            let at_ns = rng.below(duration.max(1));
            out.push(Churn {
                at_ns,
                node,
                join: false,
            });
            out.push(Churn {
                at_ns: at_ns + away_ns,
                node,
                join: true,
            });
        }
        out.sort_by_key(|c| (c.at_ns, c.node, c.join));
        out
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn poisson_mean_close() {
        let p = Poisson::new(1000.0);
        let mut rng = SplitMix64::new(1);
        let n = 20_000;
        let total: u64 = (0..n).map(|_| p.gap(&mut rng).unwrap()).sum();
        let mean = total as f64 / n as f64;
        assert!((mean - 1e6).abs() < 3e4, "{mean}");
        assert_eq!(Poisson::new(0.0).gap(&mut rng), None);
    }

    #[test]
    fn zipf_skewed_and_in_range() {
        let z = Zipf::new(100, 1.2);
        let mut rng = SplitMix64::new(2);
        let mut hist = [0u32; 100];
        for _ in 0..50_000 {
            hist[z.sample(&mut rng) as usize] += 1;
        }
        assert!(hist[0] > hist[1] && hist[1] > hist[10] && hist[10] > hist[99]);
        assert_eq!(Zipf::new(1, 1.0).sample(&mut rng), 0);
        assert_eq!(Zipf::new(0, 1.0).sample(&mut rng), 0);
    }

    #[test]
    fn ranges_stay_in_object() {
        let r = Reads::new(10, 1.0, 16, 1.0, 8);
        let mut rng = SplitMix64::new(3);
        for _ in 0..1000 {
            let read = r.draw(&mut rng);
            assert!(read.pages >= 1 && read.first + read.pages <= 16);
        }
        let single = Reads::new(10, 1.0, 16, 0.0, 8);
        assert!((0..100).all(|_| single.draw(&mut rng).pages == 1));
        assert_eq!(page_id(2, 3, 16), 35);
    }

    #[test]
    fn storm_picks_distinct_nodes() {
        let s = Storm {
            at_ns: 100,
            object: 0,
            nodes: 50,
            window_ns: 10,
        };
        let out = s.expand(60, &mut SplitMix64::new(4));
        let mut ids: Vec<u32> = out.iter().map(|(_, n)| *n).collect();
        ids.sort_unstable();
        ids.dedup();
        assert_eq!(ids.len(), 50);
        assert!(out.iter().all(|(t, n)| (100..110).contains(t) && *n < 60));
        assert_eq!(s.expand(10, &mut SplitMix64::new(4)).len(), 10);
        assert!(Storm::from_json(&json!({"at_ns": 1})).is_err());
    }

    #[test]
    fn churn_pairs_sorted() {
        let c = Churn::random(10, 20, 1000, 50, &mut SplitMix64::new(5));
        assert_eq!(c.len(), 40);
        assert!(c.windows(2).all(|w| w[0].at_ns <= w[1].at_ns));
        assert_eq!(c.iter().filter(|c| c.join).count(), 20);
        assert!(Churn::from_json(&json!({"at_ns": 1, "node": 1})).is_err());
    }
}
