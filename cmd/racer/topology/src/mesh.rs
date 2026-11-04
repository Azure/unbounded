//! The per-pool peer mesh (mesh v2).
//!
//! Each pool of `k` members is split into columns. Members of a column are
//! all linked to each other, and each member links to one delegate in every
//! other column. Any two members of a pool are at most 2 links apart. See
//! `designs/racer-mesh-v2.md` for the full layout.
//!
//! Nothing here stores neighbor lists. A pool keeps the column of each
//! member, the members of each column, one hash ring per column, and all
//! members sorted by delegate key. Neighbors are computed on demand.

use crate::{Member, hash};

use sha2::Digest;

use std::{mem::size_of, sync::Arc};

/// Pools of up to this many members are one column: a full mesh.
pub const FULL_MESH_MAX: usize = 64;

/// Ring points each member puts on its column's ring.
pub const POINTS: u32 = 16;

/// Most RDMA peers a node takes (queue pairs per device).
pub const RDMA_PEERS: usize = 128;

/// Hash prefix for a member's column choice.
const COLUMN_DOMAIN: &[u8] = b"/mesh/v2/column\0";

/// Hash prefix for a member's delegate key.
const DELEGATE_DOMAIN: &[u8] = b"/mesh/v2/delegate\0";

/// Hash prefix for a member's ring points.
const POINT_DOMAIN: &[u8] = b"/mesh/v2/point\0";

/// Hash prefix for the symmetric edge tie-break used by RDMA ranking.
const EDGE_DOMAIN: &[u8] = b"/mesh/v2/edge\0";

/// First 8 bytes of a finished hash, big-endian.
fn head(digest: sha2::Sha256) -> u64 {
    u64::from_be_bytes(hash::finish(digest)[..8].try_into().unwrap())
}

/// `H("/mesh/v2/column\0", id)`.
pub(crate) fn column_hash<M: Member>(id: &[u8]) -> u64 {
    let mut digest = hash::domain::<M>(COLUMN_DOMAIN);
    hash::bytes(&mut digest, id);
    head(digest)
}

/// `H("/mesh/v2/delegate\0", id)`.
pub(crate) fn delegate_key<M: Member>(id: &[u8]) -> u64 {
    let mut digest = hash::domain::<M>(DELEGATE_DOMAIN);
    hash::bytes(&mut digest, id);
    head(digest)
}

/// `H("/mesh/v2/point\0", id, v as u32)`.
pub(crate) fn ring_point<M: Member>(id: &[u8], point: u32) -> u64 {
    let mut digest = hash::domain::<M>(POINT_DOMAIN);
    hash::bytes(&mut digest, id);
    digest.update(point.to_be_bytes());
    head(digest)
}

/// `H("/mesh/v2/edge\0", min(a, b), max(a, b))`. Symmetric in `a` and `b`.
pub(crate) fn edge_hash<M: Member>(a: &[u8], b: &[u8]) -> u64 {
    let (low, high) = if a <= b { (a, b) } else { (b, a) };
    let mut digest = hash::domain::<M>(EDGE_DOMAIN);
    hash::bytes(&mut digest, low);
    hash::bytes(&mut digest, high);
    head(digest)
}

/// Jump consistent hash (Lamping and Veach): a bucket in `0..buckets`.
/// Growing `buckets` by one moves about `1 / buckets` of the keys.
///
/// # Panics
/// Panics if `buckets` is zero.
#[must_use]
pub fn jump(mut key: u64, buckets: u32) -> u32 {
    assert!(buckets > 0, "jump hash needs at least one bucket");
    let mut bucket: i64 = -1;
    let mut next: i64 = 0;
    while next < i64::from(buckets) {
        bucket = next;
        key = key.wrapping_mul(2_862_933_555_777_941_757).wrapping_add(1);
        #[allow(clippy::cast_precision_loss, clippy::cast_possible_truncation)]
        {
            next = ((bucket + 1) as f64 * ((1u64 << 31) as f64 / ((key >> 33) + 1) as f64)) as i64;
        }
    }
    u32::try_from(bucket).unwrap()
}

/// Target column count `C` and column size `T` for a pool of `k` members.
///
/// `C` is 1 up to [`FULL_MESH_MAX`], then the largest power of two with
/// `C * C <= k`, so it only changes when `k` crosses a power of four.
/// `T = ceil(k / C)`.
#[must_use]
pub fn target_columns(k: usize) -> (usize, usize) {
    if k <= FULL_MESH_MAX {
        return (1, k);
    }
    let columns = 1usize << (k.ilog2() / 2);
    (columns, k.div_ceil(columns))
}

/// `round(a / t)` with ties up, in integers.
fn round_div(a: usize, t: usize) -> usize {
    (2 * a + t) / (2 * t)
}

/// One column's hash ring: points sorted ascending, with their owners.
#[derive(Debug, Default)]
struct Ring {
    /// Point values, ascending.
    points: Vec<u64>,

    /// Local index owning each point.
    owners: Vec<u32>,
}

impl Ring {
    /// Owner of the first point at or after `key`, wrapping.
    fn owner(&self, key: u64) -> u32 {
        let index = self.points.partition_point(|&point| point < key);
        self.owners[if index == self.points.len() { 0 } else { index }]
    }
}

/// How two members of one pool are linked.
#[derive(Clone, Copy, Debug, Eq, Ord, PartialEq, PartialOrd)]
pub enum EdgeKind {
    /// One links to the other as its delegate in the other's column.
    Delegate,

    /// Both are in the same column.
    Column,
}

/// The mesh of one pool. Depends only on the pool's member IDs and Sites.
#[derive(Debug, Default)]
pub(crate) struct Mesh {
    /// Global positions of the members, ascending. Local index = index here.
    members: Vec<usize>,

    /// Column of each local member.
    column_of: Vec<u32>,

    /// Delegate key of each local member.
    key_of: Vec<u64>,

    /// Local members of each column, ascending.
    columns: Vec<Vec<u32>>,

    /// One ring per column. Empty with a single column.
    rings: Vec<Ring>,

    /// Every local member sorted by `(key, local)`.
    by_key: Vec<(u64, u32)>,

    /// Upper bound on incoming delegate edges of any member.
    max_incoming: usize,

    /// Target `(C, T)` for the pool size.
    target: (usize, usize),
}

impl Mesh {
    /// Build the mesh for members at `positions` (ascending, unique).
    pub(crate) fn build<M: Member>(
        positions: Vec<usize>,
        ids: &[Box<[u8]>],
        sites: &[Box<[u8]>],
    ) -> Self {
        let k = positions.len();
        let target = target_columns(k);
        let mut mesh = Self {
            column_of: vec![0; k],
            target,
            ..Self::default()
        };
        if k <= FULL_MESH_MAX {
            mesh.columns = vec![(0..k as u32).collect()];
            mesh.members = positions;
            if k == 0 {
                mesh.columns.clear();
            }
            return mesh;
        }
        let t = target.1;
        let half = t.div_ceil(2);
        // Group by Site, in Site byte order. Local order breaks ties.
        let mut by_site: Vec<u32> = (0..k as u32).collect();
        by_site.sort_by(|&a, &b| sites[positions[a as usize]].cmp(&sites[positions[b as usize]]));
        let hashes: Vec<u64> = positions
            .iter()
            .map(|&p| column_hash::<M>(&ids[p]))
            .collect();
        let mut small = Vec::new();
        let mut base = 0u32;
        let mut start = 0;
        while start < k {
            let site = &sites[positions[by_site[start] as usize]];
            let end = start
                + by_site[start..]
                    .iter()
                    .take_while(|&&l| &sites[positions[l as usize]] == site)
                    .count();
            let group = &by_site[start..end];
            if group.len() >= half {
                let count = u32::try_from(round_div(group.len(), t).max(1)).unwrap();
                for &local in group {
                    mesh.column_of[local as usize] = base + jump(hashes[local as usize], count);
                }
                base += count;
            } else {
                small.extend_from_slice(group);
            }
            start = end;
        }
        if small.len() >= half {
            let count = u32::try_from(round_div(small.len(), t).max(1)).unwrap();
            for &local in &small {
                mesh.column_of[local as usize] = base + jump(hashes[local as usize], count);
            }
            base += count;
        } else if base > 0 {
            for &local in &small {
                mesh.column_of[local as usize] = jump(hashes[local as usize], base);
            }
        } else {
            base = 1;
        }
        mesh.columns = vec![Vec::new(); base as usize];
        for local in 0..k as u32 {
            mesh.columns[mesh.column_of[local as usize] as usize].push(local);
        }
        // Rings, keys.
        mesh.key_of = positions
            .iter()
            .map(|&p| delegate_key::<M>(&ids[p]))
            .collect();
        mesh.by_key = mesh
            .key_of
            .iter()
            .enumerate()
            .map(|(local, &key)| (key, local as u32))
            .collect();
        mesh.by_key.sort_unstable();
        mesh.rings = mesh
            .columns
            .iter()
            .map(|column| {
                let mut points: Vec<(u64, u32)> = column
                    .iter()
                    .flat_map(|&local| {
                        let id = &ids[positions[local as usize]];
                        (0..POINTS).map(move |v| (ring_point::<M>(id, v), local))
                    })
                    .collect();
                // Local order follows ID order, so ties are deterministic.
                points.sort_unstable();
                Ring {
                    points: points.iter().map(|p| p.0).collect(),
                    owners: points.iter().map(|p| p.1).collect(),
                }
            })
            .collect();
        let mut incoming = vec![0usize; k];
        for ring in &mesh.rings {
            for index in 0..ring.points.len() {
                let count = mesh.arc(ring, index).map(<[_]>::len).sum::<usize>();
                incoming[ring.owners[index] as usize] += count;
            }
        }
        mesh.max_incoming = incoming.into_iter().max().unwrap_or(0);
        mesh.members = positions;
        mesh
    }

    /// Members whose key falls in the arc owned by `ring` entry `index`:
    /// keys after the previous point, up to and including this one.
    fn arc<'a>(&'a self, ring: &Ring, index: usize) -> impl Iterator<Item = &'a [(u64, u32)]> {
        let point = ring.points[index];
        let upto = self.by_key.partition_point(|e| e.0 <= point);
        let (first, second) = if index == 0 {
            let last = *ring.points.last().unwrap();
            let after = self.by_key.partition_point(|e| e.0 <= last);
            (&self.by_key[..upto], &self.by_key[after.max(upto)..])
        } else {
            let previous = ring.points[index - 1];
            let from = self.by_key.partition_point(|e| e.0 <= previous);
            (&self.by_key[from..upto.max(from)], &self.by_key[..0])
        };
        [first, second].into_iter()
    }

    /// Global positions, ascending.
    pub(crate) fn members(&self) -> &[usize] {
        &self.members
    }

    /// Local index of a global position.
    fn local(&self, position: usize) -> Option<usize> {
        self.members.binary_search(&position).ok()
    }

    /// Number of columns.
    fn column_count(&self) -> usize {
        self.columns.len()
    }

    /// Delegate (local) of local `n` in column `c`. `c` must not be `n`'s.
    fn delegate_local(&self, n: usize, column: usize) -> usize {
        self.rings[column].owner(self.key_of[n]) as usize
    }

    /// How local `a` and `b` are linked, if at all.
    fn edge_local(&self, a: usize, b: usize) -> Option<EdgeKind> {
        if a == b {
            return None;
        }
        let (ca, cb) = (self.column_of[a] as usize, self.column_of[b] as usize);
        if ca == cb {
            Some(EdgeKind::Column)
        } else if self.delegate_local(a, cb) == b || self.delegate_local(b, ca) == a {
            Some(EdgeKind::Delegate)
        } else {
            None
        }
    }

    /// Neighbors (global, unsorted, may repeat) of local `n`.
    fn neighbors_local(&self, n: usize, out: &mut Vec<usize>) {
        let own = self.column_of[n] as usize;
        out.extend(
            self.columns[own]
                .iter()
                .filter(|&&m| m as usize != n)
                .map(|&m| self.members[m as usize]),
        );
        if self.rings.is_empty() {
            return;
        }
        for column in (0..self.column_count()).filter(|&c| c != own) {
            out.push(self.members[self.delegate_local(n, column)]);
        }
        let ring = &self.rings[own];
        for index in (0..ring.owners.len()).filter(|&i| ring.owners[i] as usize == n) {
            for slice in self.arc(ring, index) {
                out.extend(
                    slice
                        .iter()
                        .filter(|e| self.column_of[e.1 as usize] as usize != own)
                        .map(|e| self.members[e.1 as usize]),
                );
            }
        }
    }

    /// Upper bound on any member's degree in this pool.
    fn max_degree(&self) -> usize {
        let largest = self.columns.iter().map(Vec::len).max().unwrap_or(0);
        let bound =
            largest.saturating_sub(1) + self.column_count().saturating_sub(1) + self.max_incoming;
        bound.min(self.members.len().saturating_sub(1))
    }

    /// Estimated heap bytes.
    pub(crate) fn bytes(&self) -> usize {
        let mut bytes = size_of::<Self>() + 2 * size_of::<usize>();
        bytes += self.members.capacity() * size_of::<usize>();
        bytes += self.column_of.capacity() * size_of::<u32>();
        bytes += self.key_of.capacity() * size_of::<u64>();
        bytes += self.columns.capacity() * size_of::<Vec<u32>>();
        bytes += self
            .columns
            .iter()
            .map(|c| c.capacity() * size_of::<u32>())
            .sum::<usize>();
        bytes += self.rings.capacity() * size_of::<Ring>();
        bytes += self
            .rings
            .iter()
            .map(|r| {
                r.points.capacity() * size_of::<u64>() + r.owners.capacity() * size_of::<u32>()
            })
            .sum::<usize>();
        bytes + self.by_key.capacity() * size_of::<(u64, u32)>()
    }
}

/// Which members a pool has. Input to [`crate::Membership::with_pools`].
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct PoolSpec {
    /// Opaque pool ID. Unique within a membership.
    pub id: Box<[u8]>,

    /// RDMA and route preference. Higher wins.
    pub rank: u64,

    /// Member IDs in the pool. `None` means every member.
    pub members: Option<Vec<Box<[u8]>>>,
}

impl PoolSpec {
    /// The default pool: every member, rank 0, ID `default`.
    #[must_use]
    pub fn default_pool() -> Self {
        Self {
            id: (*b"default").into(),
            rank: 0,
            members: None,
        }
    }
}

/// One pool of a [`crate::Membership`] and its mesh.
///
/// Positions are positions in the membership. Methods return `None` or
/// `false` for positions that are not in the pool.
#[derive(Clone, Debug)]
pub struct Pool {
    /// Opaque pool ID.
    id: Box<[u8]>,

    /// Pool rank.
    rank: u64,

    /// The mesh, shared with equal pools and predecessors.
    mesh: Arc<Mesh>,
}

impl Pool {
    /// Wrap a mesh.
    pub(crate) fn new(id: Box<[u8]>, rank: u64, mesh: Arc<Mesh>) -> Self {
        Self { id, rank, mesh }
    }

    /// The shared mesh.
    pub(crate) fn mesh(&self) -> &Arc<Mesh> {
        &self.mesh
    }

    /// Opaque pool ID.
    #[must_use]
    pub fn id(&self) -> &[u8] {
        &self.id
    }

    /// Pool rank. Higher wins.
    #[must_use]
    pub fn rank(&self) -> u64 {
        self.rank
    }

    /// Member positions, ascending.
    #[must_use]
    pub fn members(&self) -> &[usize] {
        self.mesh.members()
    }

    /// Whether `position` is in the pool.
    #[must_use]
    pub fn contains(&self, position: usize) -> bool {
        self.mesh.local(position).is_some()
    }

    /// Target column count `C` and column size `T` ([`target_columns`]).
    #[must_use]
    pub fn target(&self) -> (usize, usize) {
        self.mesh.target
    }

    /// Number of columns. Zero for an empty pool.
    #[must_use]
    pub fn columns(&self) -> usize {
        self.mesh.column_count()
    }

    /// Member positions of a column, ascending. Empty if out of range.
    #[must_use]
    pub fn column_members(&self, column: usize) -> Vec<usize> {
        self.mesh.columns.get(column).map_or_else(Vec::new, |c| {
            c.iter().map(|&l| self.mesh.members[l as usize]).collect()
        })
    }

    /// Column of a member.
    #[must_use]
    pub fn column(&self, position: usize) -> Option<usize> {
        self.mesh
            .local(position)
            .map(|l| self.mesh.column_of[l] as usize)
    }

    /// Whether both are in the pool and in the same column.
    #[must_use]
    pub fn same_column(&self, a: usize, b: usize) -> bool {
        matches!((self.column(a), self.column(b)), (Some(x), Some(y)) if x == y)
    }

    /// The member of `column` that `position` links to. `None` if the
    /// member is not in the pool, the column is out of range, or it is the
    /// member's own column.
    #[must_use]
    pub fn delegate(&self, position: usize, column: usize) -> Option<usize> {
        let local = self.mesh.local(position)?;
        if column >= self.columns() || column == self.mesh.column_of[local] as usize {
            return None;
        }
        Some(self.mesh.members[self.mesh.delegate_local(local, column)])
    }

    /// Middle node of the primary path `from -> delegate(from, col(to)) -> to`.
    /// `None` if either is not in the pool or they share a column.
    #[must_use]
    pub fn primary(&self, from: usize, to: usize) -> Option<usize> {
        self.delegate(from, self.column(to)?)
    }

    /// Middle node of the alternate path `from -> delegate(to, col(from)) -> to`.
    /// `None` if either is not in the pool or they share a column.
    #[must_use]
    pub fn alternate(&self, from: usize, to: usize) -> Option<usize> {
        self.delegate(to, self.column(from)?)
    }

    /// The hot-key helper of `reader` for pages owned by `owner`: the middle
    /// node of the reader's primary path. `None` if they share a column or
    /// either is not in the pool.
    #[must_use]
    pub fn helper(&self, reader: usize, owner: usize) -> Option<usize> {
        self.primary(reader, owner)
    }

    /// How two members are linked in this pool, if at all.
    #[must_use]
    pub fn edge(&self, a: usize, b: usize) -> Option<EdgeKind> {
        self.mesh
            .edge_local(self.mesh.local(a)?, self.mesh.local(b)?)
    }

    /// Neighbors of a member in this pool, sorted. Empty if not in the pool.
    #[must_use]
    pub fn neighbors(&self, position: usize) -> Vec<usize> {
        let mut out = Vec::new();
        self.extend_neighbors(position, &mut out);
        out.sort_unstable();
        out.dedup();
        out
    }

    /// Append neighbors (unsorted, may repeat).
    pub(crate) fn extend_neighbors(&self, position: usize, out: &mut Vec<usize>) {
        if let Some(local) = self.mesh.local(position) {
            self.mesh.neighbors_local(local, out);
        }
    }

    /// Upper bound on any member's degree in this pool.
    #[must_use]
    pub fn max_degree(&self) -> usize {
        self.mesh.max_degree()
    }
}

/// Mesh tests that need private state.
#[cfg(test)]
mod tests {
    use super::*;

    /// Reference values for jump hashing from the paper's algorithm.
    #[test]
    fn jump_is_stable_and_moves_few_keys() {
        assert_eq!(jump(0, 1), 0);
        let mut moved = 0;
        for key in 0..10_000u64 {
            let key = key.wrapping_mul(0x9e37_79b9_7f4a_7c15);
            let a = jump(key, 10);
            let b = jump(key, 11);
            assert!(a < 10 && b < 11);
            if a != b {
                assert_eq!(b, 10, "keys only move to the new bucket");
                moved += 1;
            }
        }
        assert!((700..1100).contains(&moved), "{moved}");
    }

    /// `C` only changes at powers of four and matches the spec table.
    #[test]
    fn columns_change_only_at_powers_of_four() {
        for (k, c, t) in [
            (64, 1, 64),
            (65, 8, 9),
            (256, 16, 16),
            (1024, 32, 32),
            (4096, 64, 64),
            (20_000, 128, 157),
            (100_000, 256, 391),
        ] {
            assert_eq!(target_columns(k), (c, t), "k={k}");
        }
        let mut previous = target_columns(65).0;
        for k in 66..70_000 {
            let c = target_columns(k).0;
            if c != previous {
                assert!(k.is_power_of_two() && k.trailing_zeros() % 2 == 0, "k={k}");
                assert_eq!(c, 2 * previous);
            }
            previous = c;
        }
    }

    /// Rounding helpers use integer math.
    #[test]
    fn integer_rounding() {
        assert_eq!(round_div(5, 10), 1);
        assert_eq!(round_div(4, 10), 0);
        assert_eq!(round_div(15, 10), 2);
        assert_eq!(round_div(14, 10), 1);
    }
}
