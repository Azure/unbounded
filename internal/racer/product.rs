//! Pure, deterministic Cartesian-product topology shared by the Rust daemons via `#[path]`.
//!
//! Roles are `left * right.order() + right`. Paths include both endpoints and count
//! hops as `path.len() - 1`. This module models logical roles, not physical bundles.

use std::collections::VecDeque;
use std::sync::OnceLock;

/// A diameter-at-most-two factor. Clique orders must be in `1..=32`.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Hash)]
pub enum Factor {
    Clique(u32),
    HoffmanSingleton,
    Abas,
}

impl Factor {
    fn from_code(code: u32) -> Result<Self, String> {
        match code {
            1..=32 => Ok(Self::Clique(code)),
            50 => Ok(Self::HoffmanSingleton),
            200 => Ok(Self::Abas),
            _ => Err(format!("unsupported factor code {code}")),
        }
    }

    fn valid(self) -> bool {
        !matches!(self, Self::Clique(n) if !(1..=32).contains(&n))
    }

    /// Number of vertices; also the factor's wire code.
    ///
    /// Panics for a directly constructed invalid clique.
    pub fn order(&self) -> u32 {
        assert!(self.valid(), "clique order must be in 1..=32");
        match self {
            Self::Clique(n) => *n,
            Self::HoffmanSingleton => 50,
            Self::Abas => 200,
        }
    }

    fn degree(self) -> u32 {
        match self {
            Self::Clique(_) => self.order() - 1,
            Self::HoffmanSingleton => 7,
            Self::Abas => 16,
        }
    }

    fn table(self) -> &'static Table {
        static HS: OnceLock<Table> = OnceLock::new();
        static ABAS: OnceLock<Table> = OnceLock::new();
        match self {
            Self::HoffmanSingleton => HS.get_or_init(|| Table::new(hoffman_singleton())),
            Self::Abas => ABAS.get_or_init(|| Table::new(abas())),
            Self::Clique(_) => unreachable!("cliques need no tables"),
        }
    }

    /// Sorted, distinct adjacent vertices. Panics if `v` or the factor is invalid.
    pub fn neighbors(&self, v: u32) -> Vec<u32> {
        assert!(v < self.order(), "factor vertex out of range");
        match self {
            Self::Clique(n) => (0..*n).filter(|&w| w != v).collect(),
            _ => self.table().adjacency[v as usize].clone(),
        }
    }

    /// Exact shortest distance. Panics if either vertex or the factor is invalid.
    pub fn distance(&self, s: u32, t: u32) -> u32 {
        let n = self.order();
        assert!(s < n && t < n, "factor vertex out of range");
        match self {
            Self::Clique(_) => u32::from(s != t),
            _ => self.table().distance[(s * n + t) as usize] as u32,
        }
    }

    /// Lowest-numbered neighbor on a shortest path, or `s` when `s == t`.
    /// Panics if either vertex or the factor is invalid.
    pub fn next(&self, s: u32, t: u32) -> u32 {
        let n = self.order();
        assert!(s < n && t < n, "factor vertex out of range");
        match self {
            Self::Clique(_) => t,
            _ => self.table().next[(s * n + t) as usize] as u32,
        }
    }
}

struct Table {
    adjacency: Vec<Vec<u32>>,
    distance: Vec<u8>,
    next: Vec<u8>,
}

impl Table {
    fn new(mut adjacency: Vec<Vec<u32>>) -> Self {
        for neighbors in &mut adjacency {
            neighbors.sort_unstable();
            neighbors.dedup();
        }
        let n = adjacency.len();
        let mut distance = vec![u8::MAX; n * n];
        for s in 0..n {
            let row = &mut distance[s * n..(s + 1) * n];
            row[s] = 0;
            let mut queue = VecDeque::from([s]);
            while let Some(v) = queue.pop_front() {
                for &w in &adjacency[v] {
                    let w = w as usize;
                    if row[w] == u8::MAX {
                        row[w] = row[v] + 1;
                        queue.push_back(w);
                    }
                }
            }
            assert!(row.iter().all(|&d| d <= 2), "factor diameter exceeds two");
        }
        let mut next = vec![0; n * n];
        for s in 0..n {
            for t in 0..n {
                next[s * n + t] = if s == t {
                    s as u8
                } else {
                    *adjacency[s]
                        .iter()
                        .find(|&&w| distance[w as usize * n + t] + 1 == distance[s * n + t])
                        .expect("connected factor") as u8
                };
            }
        }
        Self {
            adjacency,
            distance,
            next,
        }
    }
}

fn hoffman_singleton() -> Vec<Vec<u32>> {
    // P(i,j) = 5*i+j: five pentagons. Q(k,l) = 25+5*k+l: five pentagrams.
    // Cross edges join P(i,j) to Q(k, i*k+j mod 5).
    let mut adjacency = vec![Vec::new(); 50];
    for i in 0..5 {
        for j in 0..5 {
            let p = 5 * i + j;
            adjacency[p].extend([5 * i + (j + 1) % 5, 5 * i + (j + 4) % 5].map(|v| v as u32));
            let q = 25 + p;
            adjacency[q]
                .extend([25 + 5 * i + (j + 2) % 5, 25 + 5 * i + (j + 3) % 5].map(|v| v as u32));
            for k in 0..5 {
                let q = 25 + 5 * k + (i * k + j) % 5;
                adjacency[p].push(q as u32);
                adjacency[q].push(p as u32);
            }
        }
    }
    adjacency
}

fn abas() -> Vec<Vec<u32>> {
    // Abas, https://arxiv.org/html/1509.00842v4#S4, Theorem 4.1.
    // (Z10 x Z10) semidirect Z2, where the nonidentity Z2 element swaps
    // coordinates. Encode (x,y,i) as 2*(10*x+y)+i and use right multiplication.
    let seeds = [
        (0, 0, 1), // A
        (1, 0, 1), // B
        (1, 3, 1),
        (1, 7, 1),
        (5, 0, 1),
        (5, 2, 1),
        (5, 0, 0), // C
        (4, 1, 0),
        (3, 2, 0),
    ];
    let mut generators = Vec::new();
    for (x, y, i) in seeds {
        generators.push((x, y, i));
        let (x, y) = if i == 0 { (x, y) } else { (y, x) };
        generators.push(((10 - x) % 10, (10 - y) % 10, i));
    }
    generators.sort_unstable();
    generators.dedup();
    (0..200)
        .map(|v| {
            let (x, y, i) = (v / 20, (v / 2) % 10, v % 2);
            generators
                .iter()
                .map(|&(a, b, j)| {
                    let (a, b) = if i == 0 { (a, b) } else { (b, a) };
                    2 * (10 * ((x + a) % 10) + (y + b) % 10) + (i ^ j)
                })
                .collect()
        })
        .collect()
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, Hash)]
pub struct Product {
    pub left: Factor,
    pub right: Factor,
}

impl Product {
    pub fn new(left_code: u32, right_code: u32) -> Result<Self, String> {
        Ok(Self {
            left: Factor::from_code(left_code)?,
            right: Factor::from_code(right_code)?,
        })
    }

    pub fn codes(&self) -> (u32, u32) {
        (self.left.order(), self.right.order())
    }

    pub fn order(&self) -> u32 {
        self.left.order() * self.right.order()
    }

    /// Admission for independent physical bundles. A bundled role needs at
    /// least two neighboring roles, so one failed physical intermediate cannot
    /// isolate two surviving endpoints in that bundle. In particular K2 is
    /// permitted only for exactly two members, and K1 only for one member.
    pub fn supports_members(&self, member_count: u32) -> bool {
        if !self.left.valid() || !self.right.valid() || !(1..=100_000).contains(&member_count) {
            return false;
        }
        let order = self.order();
        order <= member_count
            && (order == member_count || self.left.degree() + self.right.degree() >= 2)
    }

    fn validate(&self, roles: &[u32]) -> Result<(), String> {
        if !self.left.valid() || !self.right.valid() {
            return Err("clique order must be in 1..=32".into());
        }
        for &role in roles {
            if role >= self.order() {
                return Err(format!(
                    "role {role} outside product order {}",
                    self.order()
                ));
            }
        }
        Ok(())
    }

    /// Sorted, distinct adjacent roles. Panics for an invalid role or factor.
    pub fn neighbors(&self, role: u32) -> Vec<u32> {
        assert!(role < self.order(), "product role out of range");
        let n = self.right.order();
        let (left, right) = (role / n, role % n);
        let mut neighbors: Vec<_> = self
            .left
            .neighbors(left)
            .into_iter()
            .map(|v| v * n + right)
            .chain(
                self.right
                    .neighbors(right)
                    .into_iter()
                    .map(|v| left * n + v),
            )
            .collect();
        neighbors.sort_unstable();
        neighbors
    }

    /// Healthy shortest path, dynamically taking a distance-two coordinate first.
    /// Ties select the left coordinate. Reevaluate after every hop: completing a
    /// coordinate before considering the other can break the prefix repair bound.
    pub fn route(&self, source: u32, target: u32) -> Result<Vec<u32>, String> {
        self.validate(&[source, target])?;
        let n = self.right.order();
        let (mut left, mut right) = (source / n, source % n);
        let (to_left, to_right) = (target / n, target % n);
        let mut path = vec![source];
        while left != to_left || right != to_right {
            let dl = self.left.distance(left, to_left);
            let dr = self.right.distance(right, to_right);
            if dl == 2 || (dr != 2 && dl == 1) {
                left = self.left.next(left, to_left);
            } else {
                right = self.right.next(right, to_right);
            }
            path.push(left * n + right);
        }
        Ok(path)
    }

    /// Shortest path of at most four hops avoiding one failed role.
    ///
    /// The source is the current role, not necessarily the original source. The
    /// caller tracks already traversed hops. A healthy route's internal next-hop
    /// failure permits a repair with prefix plus suffix at most four hops. Failed
    /// endpoints and out-of-range roles are errors, even for a zero-hop route.
    /// BFS uses O(product order) scratch space; no product adjacency is cached.
    pub fn repair(&self, source: u32, target: u32, failed: u32) -> Result<Vec<u32>, String> {
        self.validate(&[source, target, failed])?;
        if source == failed || target == failed {
            return Err("cannot route from or to the failed role".into());
        }
        let mut parent = vec![u32::MAX; self.order() as usize];
        let mut queue = VecDeque::from([(source, 0)]);
        parent[source as usize] = source;
        while let Some((v, depth)) = queue.pop_front() {
            if v == target {
                let mut path = vec![v];
                let mut current = v;
                while current != source {
                    current = parent[current as usize];
                    path.push(current);
                }
                path.reverse();
                return Ok(path);
            }
            if depth == 4 {
                continue;
            }
            for w in self.neighbors(v) {
                if w != failed && parent[w as usize] == u32::MAX {
                    parent[w as usize] = v;
                    queue.push_back((w, depth + 1));
                }
            }
        }
        Err("no path of at most four hops avoiding the failed role".into())
    }
}

/// Select a base graph for `1..=100_000` physical members.
///
/// Minimize `ceil(member_count / order) * (left_degree + right_degree)` over
/// bases no larger than the membership; break ties by largest base, then smallest
/// `(left_code, right_code)`. Bundled bases require degree at least two to avoid
/// a sole physical intermediate between same-role endpoints. K1 is eligible
/// only for one member and K2 only for exactly two members.
pub fn choose(member_count: u32) -> Result<Product, String> {
    if !(1..=100_000).contains(&member_count) {
        return Err("physical member count must be in 1..=100000".into());
    }
    let codes: Vec<_> = (1..=32).chain([50, 200]).collect();
    let mut best: Option<(u32, u32, Product)> = None;
    for &left in &codes {
        for &right in &codes {
            let product = Product::new(left, right)?;
            let order = product.order();
            if !product.supports_members(member_count) {
                continue;
            }
            let cost =
                member_count.div_ceil(order) * (product.left.degree() + product.right.degree());
            if best.as_ref().is_none_or(|&(best_cost, best_order, _)| {
                cost < best_cost || (cost == best_cost && order > best_order)
            }) {
                best = Some((cost, order, product));
            }
        }
    }
    Ok(best
        .expect("every supported membership has an eligible base")
        .2)
}

#[cfg(test)]
#[path = "product_tests.rs"]
mod tests;
