//! In-memory copy-on-write B+tree index and its I/O-free mount loader.
//!
//! The whole tree lives in memory. Nodes mirror the on-disk format in
//! [`super::node`]. A commit encodes every dirty node, children before parents,
//! to newly allocated addresses. See `designs/racer-extent-volume.md`.

use std::collections::{HashMap, HashSet};
use std::ops::Bound;

use super::format::{Key, NodeAddr, UNIT_BYTES, Value};
use super::node::{
    self, DecodedNode, Expect, INNER_CAPACITY, LEAF_CAPACITY, NodeBody, encode_inner, encode_leaf,
};
use crate::{Error, Result};

/// Arena index meaning "no node".
const NIL: u32 = u32::MAX;

/// Loader placeholder for a child whose range was lost.
const LOST: u32 = u32::MAX - 1;

/// Loader placeholder for a child not loaded yet.
const PENDING: u32 = u32::MAX - 2;

/// Non-root leaves are rebalanced below this many entries.
const LEAF_MIN: usize = LEAF_CAPACITY / 4;

/// Non-root inner nodes are rebalanced below this many children.
const INNER_MIN: usize = INNER_CAPACITY / 4;

const NO_ADDRS: [NodeAddr; 2] = [NodeAddr::NONE, NodeAddr::NONE];

#[derive(Debug)]
struct Node {
    level: u8,
    live: bool,
    dirty: bool,
    /// Changes on every modification, allocation, and free.
    stamp: u64,
    /// Durable addresses while clean, `NONE` while dirty.
    addrs: [NodeAddr; 2],
    /// Leaf keys, or the lowest key of each child.
    keys: Vec<Key>,
    /// Leaf values. Empty for inner nodes.
    values: Vec<Value>,
    /// Children. Empty for leaves.
    children: Vec<u32>,
}

impl Node {
    fn capacity(&self) -> usize {
        if self.level == 0 {
            LEAF_CAPACITY
        } else {
            INNER_CAPACITY
        }
    }

    fn min(&self) -> usize {
        if self.level == 0 { LEAF_MIN } else { INNER_MIN }
    }
}

/// Index of the child of an inner node that covers `key`.
fn child_index(keys: &[Key], key: &Key) -> usize {
    keys.partition_point(|k| k <= key).saturating_sub(1)
}

/// In-memory copy-on-write B+tree from [`Key`] to [`Value`].
///
/// Values must be well formed ([`Value::is_well_formed`]); otherwise their leaf
/// would fail to load.
#[derive(Debug)]
pub struct Tree {
    volume_id: [u8; 16],
    nodes: Vec<Node>,
    free_nodes: Vec<u32>,
    root: u32,
    len: u64,
    stamp: u64,
    freed: Vec<NodeAddr>,
    /// Bumped whenever the root node changes identity.
    root_change: u64,
    /// `root_change` of the last durable root.
    committed_root_change: u64,
}

/// Nodes and root produced by [`Tree::prepare_commit`].
///
/// Pass it back to [`Tree::commit_succeeded`] or [`Tree::commit_failed`].
#[derive(Debug)]
pub struct CommitPlan {
    /// Seq written into every node.
    pub seq: u64,

    /// Node images to write, keyed by address.
    pub writes: Vec<(NodeAddr, Box<[u8; UNIT_BYTES]>)>,

    /// Root addresses for the superblock. `NONE` twice for an empty tree.
    pub root: [NodeAddr; 2],

    /// Root level for the superblock.
    pub root_level: u8,

    /// Entry count for the superblock.
    pub entries: u64,

    nodes: Vec<(u32, u64, [NodeAddr; 2])>,
    root_change: u64,
}

impl Tree {
    /// Empty tree.
    pub fn new(volume_id: [u8; 16]) -> Self {
        Self {
            volume_id,
            nodes: Vec::new(),
            free_nodes: Vec::new(),
            root: NIL,
            len: 0,
            stamp: 0,
            freed: Vec::new(),
            root_change: 0,
            committed_root_change: 0,
        }
    }

    /// Volume id written into nodes.
    pub fn volume_id(&self) -> [u8; 16] {
        self.volume_id
    }

    /// Number of entries.
    pub fn len(&self) -> u64 {
        self.len
    }

    /// Whether the tree has no entries.
    pub fn is_empty(&self) -> bool {
        self.len == 0
    }

    /// Value of `key`.
    pub fn get(&self, key: &Key) -> Option<Value> {
        let leaf = self.find_leaf(key)?;
        let node = &self.nodes[leaf as usize];
        node.keys.binary_search(key).ok().map(|i| node.values[i])
    }

    /// Insert or replace. Returns the previous value.
    pub fn insert(&mut self, key: Key, value: Value) -> Option<Value> {
        debug_assert!(value.is_well_formed());
        if self.root == NIL {
            let root = self.alloc_node(0);
            let node = &mut self.nodes[root as usize];
            node.keys.push(key);
            node.values.push(value);
            self.set_root(root);
            self.len = 1;
            return None;
        }
        let (old, split) = self.insert_rec(self.root, key, value);
        if let Some((separator, right)) = split {
            let left = self.root;
            let level = self.nodes[left as usize].level + 1;
            let first = self.nodes[left as usize].keys[0];
            let root = self.alloc_node(level);
            let node = &mut self.nodes[root as usize];
            node.keys.extend([first, separator]);
            node.children.extend([left, right]);
            self.set_root(root);
        }
        if old.is_none() {
            self.len += 1;
        }
        old
    }

    /// Remove `key`. Returns its value.
    pub fn remove(&mut self, key: &Key) -> Option<Value> {
        if self.root == NIL {
            return None;
        }
        let old = self.remove_rec(self.root, key)?;
        self.len -= 1;
        self.fix_root();
        Some(old)
    }

    /// Change the value of `key` in place. Returns whether it was found. The
    /// tree is modified only if it was.
    pub fn update(&mut self, key: &Key, f: impl FnOnce(&mut Value)) -> bool {
        let Some(leaf) = self.find_leaf(key) else {
            return false;
        };
        let Ok(pos) = self.nodes[leaf as usize].keys.binary_search(key) else {
            return false;
        };
        let mut idx = self.root;
        loop {
            self.touch(idx);
            let node = &self.nodes[idx as usize];
            if node.level == 0 {
                break;
            }
            idx = node.children[child_index(&node.keys, key)];
        }
        f(&mut self.nodes[leaf as usize].values[pos]);
        debug_assert!(self.nodes[leaf as usize].values[pos].is_well_formed());
        true
    }

    /// Entries with keys in `(low, high)`, in key order.
    pub fn range(&self, low: Bound<Key>, high: Bound<Key>) -> Range<'_> {
        let (leaf, pos, upper) = self.seek(low.as_ref());
        Range {
            tree: self,
            leaf,
            pos,
            upper,
            high,
        }
    }

    /// Up to `limit` entries with keys after `after` (from the start if `None`).
    pub fn next_after(&self, after: Option<&Key>, limit: usize) -> Vec<(Key, Value)> {
        let low = after.map_or(Bound::Unbounded, |k| Bound::Excluded(*k));
        self.range(low, Bound::Unbounded).take(limit).collect()
    }

    /// Whether a commit would change anything on disk.
    pub fn is_dirty(&self) -> bool {
        self.root_change != self.committed_root_change
            || (self.root != NIL && self.nodes[self.root as usize].dirty)
    }

    /// Durable addresses superseded since the last call: nodes that became
    /// dirty or were deleted, and addresses of failed or outdated commit plans.
    pub fn take_freed(&mut self) -> Vec<NodeAddr> {
        std::mem::take(&mut self.freed)
    }

    /// Durable addresses of all clean nodes.
    pub fn node_addrs(&self) -> impl Iterator<Item = NodeAddr> + '_ {
        self.nodes
            .iter()
            .filter(|n| n.live && !n.dirty)
            .flat_map(|n| n.addrs)
            .filter(|a| !a.is_none())
    }

    /// Number of nodes in memory.
    pub fn node_count(&self) -> usize {
        self.nodes.len() - self.free_nodes.len()
    }

    /// Number of levels, 0 for an empty tree.
    pub fn height(&self) -> u8 {
        if self.root == NIL {
            0
        } else {
            self.nodes[self.root as usize].level + 1
        }
    }

    /// Encode every dirty node with `seq` at addresses from `alloc` (one per
    /// leaf, two per inner node). The tree is not changed. On error, addresses
    /// already allocated are reported by [`Self::take_freed`].
    pub fn prepare_commit(
        &mut self,
        seq: u64,
        alloc: &mut dyn FnMut() -> Result<NodeAddr>,
    ) -> Result<CommitPlan> {
        let mut plan = CommitPlan {
            seq,
            writes: Vec::new(),
            root: NO_ADDRS,
            root_level: 0,
            entries: self.len,
            nodes: Vec::new(),
            root_change: self.root_change,
        };
        if self.root == NIL {
            return Ok(plan);
        }
        let mut allocated = Vec::new();
        match self.plan_node(self.root, alloc, &mut plan, &mut allocated) {
            Ok(root) => {
                plan.root = root;
                plan.root_level = self.nodes[self.root as usize].level;
                Ok(plan)
            }
            Err(error) => {
                self.freed.extend(allocated);
                Err(error)
            }
        }
    }

    /// The plan is durable. Its nodes become clean at their new addresses,
    /// except nodes changed since [`Self::prepare_commit`]; their plan
    /// addresses are reported by [`Self::take_freed`].
    pub fn commit_succeeded(&mut self, plan: CommitPlan) {
        for (idx, stamp, addrs) in plan.nodes {
            let node = &mut self.nodes[idx as usize];
            if node.live && node.stamp == stamp {
                node.dirty = false;
                node.addrs = addrs;
            } else {
                self.freed
                    .extend(addrs.into_iter().filter(|a| !a.is_none()));
            }
        }
        self.committed_root_change = plan.root_change;
    }

    /// The plan did not become durable. Nodes stay dirty, and the plan's
    /// addresses are reported by [`Self::take_freed`].
    pub fn commit_failed(&mut self, plan: CommitPlan) {
        for (_, _, addrs) in plan.nodes {
            self.freed
                .extend(addrs.into_iter().filter(|a| !a.is_none()));
        }
    }

    fn plan_node(
        &self,
        idx: u32,
        alloc: &mut dyn FnMut() -> Result<NodeAddr>,
        plan: &mut CommitPlan,
        allocated: &mut Vec<NodeAddr>,
    ) -> Result<[NodeAddr; 2]> {
        let node = &self.nodes[idx as usize];
        if !node.dirty {
            return Ok(node.addrs);
        }
        if node.level == 0 {
            let addr = alloc()?;
            if addr.is_none() {
                return Err(Error::InvalidConfiguration);
            }
            allocated.push(addr);
            let bytes = encode_leaf(&self.volume_id, plan.seq, addr, &node.keys, &node.values);
            plan.writes.push((addr, bytes));
            let addrs = [addr, NodeAddr::NONE];
            plan.nodes.push((idx, node.stamp, addrs));
            return Ok(addrs);
        }
        let mut children = Vec::with_capacity(node.children.len());
        for &child in &node.children {
            children.push(self.plan_node(child, alloc, plan, allocated)?);
        }
        let first = alloc()?;
        if first.is_none() {
            return Err(Error::InvalidConfiguration);
        }
        allocated.push(first);
        let second = alloc()?;
        if second.is_none() || second == first {
            return Err(Error::InvalidConfiguration);
        }
        allocated.push(second);
        for addr in [first, second] {
            let bytes = encode_inner(
                &self.volume_id,
                plan.seq,
                addr,
                node.level,
                &node.keys,
                &children,
            );
            plan.writes.push((addr, bytes));
        }
        let addrs = [first, second];
        plan.nodes.push((idx, node.stamp, addrs));
        Ok(addrs)
    }

    fn find_leaf(&self, key: &Key) -> Option<u32> {
        if self.root == NIL {
            return None;
        }
        let mut idx = self.root;
        loop {
            let node = &self.nodes[idx as usize];
            if node.level == 0 {
                return Some(idx);
            }
            idx = node.children[child_index(&node.keys, key)];
        }
    }

    /// First leaf position at or after `low`, plus the lowest key of the next
    /// subtree to continue with.
    fn seek(&self, low: Bound<&Key>) -> (u32, usize, Option<Key>) {
        if self.root == NIL {
            return (NIL, 0, None);
        }
        let mut idx = self.root;
        let mut upper = None;
        loop {
            let node = &self.nodes[idx as usize];
            if node.level == 0 {
                let pos = match low {
                    Bound::Unbounded => 0,
                    Bound::Included(k) => node.keys.partition_point(|x| x < k),
                    Bound::Excluded(k) => node.keys.partition_point(|x| x <= k),
                };
                return (idx, pos, upper);
            }
            let i = match low {
                Bound::Unbounded => 0,
                Bound::Included(k) | Bound::Excluded(k) => child_index(&node.keys, k),
            };
            if let Some(&next) = node.keys.get(i + 1) {
                upper = Some(next);
            }
            idx = node.children[i];
        }
    }

    fn alloc_node(&mut self, level: u8) -> u32 {
        self.stamp += 1;
        let stamp = self.stamp;
        let (keys, values, children) = if level == 0 {
            (LEAF_CAPACITY + 1, LEAF_CAPACITY + 1, 0)
        } else {
            (INNER_CAPACITY + 1, 0, INNER_CAPACITY + 1)
        };
        if let Some(idx) = self.free_nodes.pop() {
            let node = &mut self.nodes[idx as usize];
            node.level = level;
            node.live = true;
            node.dirty = true;
            node.stamp = stamp;
            node.addrs = NO_ADDRS;
            node.keys.reserve(keys);
            node.values.reserve(values);
            node.children.reserve(children);
            return idx;
        }
        let idx = u32::try_from(self.nodes.len()).expect("node arena overflow");
        assert!(idx < PENDING, "node arena overflow");
        self.nodes.push(Node {
            level,
            live: true,
            dirty: true,
            stamp,
            addrs: NO_ADDRS,
            keys: Vec::with_capacity(keys),
            values: Vec::with_capacity(values),
            children: Vec::with_capacity(children),
        });
        idx
    }

    fn free_node(&mut self, idx: u32) {
        self.stamp += 1;
        let node = &mut self.nodes[idx as usize];
        if !node.dirty {
            self.freed
                .extend(node.addrs.into_iter().filter(|a| !a.is_none()));
        }
        node.live = false;
        node.dirty = true;
        node.stamp = self.stamp;
        node.addrs = NO_ADDRS;
        node.keys.clear();
        node.values.clear();
        node.children.clear();
        self.free_nodes.push(idx);
    }

    /// Mark a node modified.
    fn touch(&mut self, idx: u32) {
        self.stamp += 1;
        let node = &mut self.nodes[idx as usize];
        node.stamp = self.stamp;
        if !node.dirty {
            node.dirty = true;
            self.freed
                .extend(node.addrs.into_iter().filter(|a| !a.is_none()));
            node.addrs = NO_ADDRS;
        }
    }

    fn set_root(&mut self, root: u32) {
        self.root = root;
        self.root_change += 1;
    }

    fn two_mut(&mut self, a: u32, b: u32) -> (&mut Node, &mut Node) {
        let (a, b) = (a as usize, b as usize);
        assert_ne!(a, b);
        if a < b {
            let (lo, hi) = self.nodes.split_at_mut(b);
            (&mut lo[a], &mut hi[0])
        } else {
            let (lo, hi) = self.nodes.split_at_mut(a);
            (&mut hi[0], &mut lo[b])
        }
    }

    fn insert_rec(
        &mut self,
        idx: u32,
        key: Key,
        value: Value,
    ) -> (Option<Value>, Option<(Key, u32)>) {
        self.touch(idx);
        let node = &mut self.nodes[idx as usize];
        if node.level == 0 {
            match node.keys.binary_search(&key) {
                Ok(i) => return (Some(std::mem::replace(&mut node.values[i], value)), None),
                Err(i) => {
                    node.keys.insert(i, key);
                    node.values.insert(i, value);
                }
            }
            if node.keys.len() > LEAF_CAPACITY {
                return (None, Some(self.split(idx)));
            }
            return (None, None);
        }
        let i = child_index(&node.keys, &key);
        if key < node.keys[0] {
            // Keep the first key a lower bound of the node's contents.
            node.keys[0] = key;
        }
        let child = node.children[i];
        let (old, split) = self.insert_rec(child, key, value);
        if let Some((separator, right)) = split {
            let node = &mut self.nodes[idx as usize];
            node.keys.insert(i + 1, separator);
            node.children.insert(i + 1, right);
            if node.keys.len() > INNER_CAPACITY {
                return (old, Some(self.split(idx)));
            }
        }
        (old, None)
    }

    /// Move the upper half of an overfull node to a new right sibling.
    fn split(&mut self, idx: u32) -> (Key, u32) {
        let level = self.nodes[idx as usize].level;
        let right = self.alloc_node(level);
        let (l, r) = self.two_mut(idx, right);
        let mid = l.keys.len() / 2;
        r.keys.extend(l.keys.drain(mid..));
        r.values.extend(l.values.drain(mid.min(l.values.len())..));
        r.children
            .extend(l.children.drain(mid.min(l.children.len())..));
        (r.keys[0], right)
    }

    fn remove_rec(&mut self, idx: u32, key: &Key) -> Option<Value> {
        let node = &self.nodes[idx as usize];
        if node.level == 0 {
            let i = node.keys.binary_search(key).ok()?;
            self.touch(idx);
            let node = &mut self.nodes[idx as usize];
            node.keys.remove(i);
            return Some(node.values.remove(i));
        }
        let i = child_index(&node.keys, key);
        let child = node.children[i];
        let old = self.remove_rec(child, key)?;
        self.touch(idx);
        let child_node = &self.nodes[child as usize];
        if child_node.keys.len() < child_node.min() {
            self.rebalance(idx, i);
        }
        Some(old)
    }

    /// Merge or even out the underfull child `i` of `parent` with a sibling.
    fn rebalance(&mut self, parent: u32, i: usize) {
        let count = self.nodes[parent as usize].children.len();
        if count < 2 {
            let child = self.nodes[parent as usize].children[i];
            if self.nodes[child as usize].keys.is_empty() {
                let p = &mut self.nodes[parent as usize];
                p.keys.remove(i);
                p.children.remove(i);
                self.free_node(child);
            }
            return;
        }
        let (li, ri) = if i > 0 { (i - 1, i) } else { (0, 1) };
        let p = &self.nodes[parent as usize];
        let (l, r, separator) = (p.children[li], p.children[ri], p.keys[ri]);
        self.touch(l);
        self.touch(r);
        let (left, right) = self.two_mut(l, r);
        if left.level > 0 && !right.keys.is_empty() {
            // The right node's first child covers keys from the separator on.
            right.keys[0] = separator;
        }
        let total = left.keys.len() + right.keys.len();
        if total <= left.capacity() {
            left.keys.append(&mut right.keys);
            left.values.append(&mut right.values);
            left.children.append(&mut right.children);
            let p = &mut self.nodes[parent as usize];
            p.keys.remove(ri);
            p.children.remove(ri);
            self.free_node(r);
            return;
        }
        let target = total / 2;
        if left.keys.len() < target {
            let n = target - left.keys.len();
            left.keys.extend(right.keys.drain(..n));
            let nv = n.min(right.values.len());
            left.values.extend(right.values.drain(..nv));
            let nc = n.min(right.children.len());
            left.children.extend(right.children.drain(..nc));
        } else {
            let at = target;
            right.keys.splice(0..0, left.keys.drain(at..));
            let av = at.min(left.values.len());
            right.values.splice(0..0, left.values.drain(av..));
            let ac = at.min(left.children.len());
            right.children.splice(0..0, left.children.drain(ac..));
        }
        let first = right.keys[0];
        self.nodes[parent as usize].keys[ri] = first;
    }

    /// Drop an empty root and collapse roots with a single child.
    fn fix_root(&mut self) {
        while self.root != NIL {
            let root = &self.nodes[self.root as usize];
            if root.keys.is_empty() {
                let old = self.root;
                self.free_node(old);
                self.set_root(NIL);
            } else if root.level > 0 && root.children.len() == 1 {
                let (old, child) = (self.root, root.children[0]);
                self.free_node(old);
                self.set_root(child);
            } else {
                break;
            }
        }
    }

    /// Check structural invariants. Panics on violation. With `fill`, also
    /// checks that non-root nodes are at least a quarter full.
    #[cfg(test)]
    pub(crate) fn check(&self, fill: bool) {
        if self.root == NIL {
            assert_eq!(self.len, 0);
            return;
        }
        let root = &self.nodes[self.root as usize];
        assert!(!root.keys.is_empty());
        let mut count = 0;
        self.check_node(
            self.root,
            Key::MIN,
            None,
            root.level,
            true,
            fill,
            &mut count,
        );
        assert_eq!(count, self.len);
        let live = self.nodes.iter().filter(|n| n.live).count();
        assert_eq!(live, self.node_count());
    }

    #[cfg(test)]
    #[allow(clippy::too_many_arguments)]
    fn check_node(
        &self,
        idx: u32,
        low: Key,
        high: Option<Key>,
        level: u8,
        is_root: bool,
        fill: bool,
        count: &mut u64,
    ) {
        let node = &self.nodes[idx as usize];
        assert!(node.live);
        assert_eq!(node.level, level);
        assert!(!node.keys.is_empty() && node.keys.len() <= node.capacity());
        if fill && !is_root {
            assert!(
                node.keys.len() >= node.min(),
                "underfull {}",
                node.keys.len()
            );
        }
        assert!(node.keys.windows(2).all(|w| w[0] < w[1]));
        assert!(node.keys[0] >= low);
        assert!(high.is_none_or(|h| *node.keys.last().unwrap() < h));
        assert_eq!(node.dirty, node.addrs[0].is_none());
        if level == 0 {
            assert_eq!(node.values.len(), node.keys.len());
            assert!(node.children.is_empty());
            *count += node.keys.len() as u64;
            return;
        }
        assert!(node.values.is_empty());
        assert_eq!(node.children.len(), node.keys.len());
        if is_root {
            assert!(node.children.len() >= 2);
        }
        for (i, &child) in node.children.iter().enumerate() {
            let child_low = if i == 0 { low } else { node.keys[i] };
            let child_high = node.keys.get(i + 1).copied().or(high);
            if self.nodes[child as usize].dirty {
                assert!(node.dirty, "dirty child under clean parent");
            }
            self.check_node(child, child_low, child_high, level - 1, false, fill, count);
        }
    }
}

/// Iterator returned by [`Tree::range`].
#[derive(Debug)]
pub struct Range<'a> {
    tree: &'a Tree,
    leaf: u32,
    pos: usize,
    upper: Option<Key>,
    high: Bound<Key>,
}

impl Iterator for Range<'_> {
    type Item = (Key, Value);

    fn next(&mut self) -> Option<Self::Item> {
        loop {
            if self.leaf == NIL {
                return None;
            }
            let node = &self.tree.nodes[self.leaf as usize];
            if self.pos < node.keys.len() {
                let key = node.keys[self.pos];
                let inside = match &self.high {
                    Bound::Unbounded => true,
                    Bound::Included(h) => key <= *h,
                    Bound::Excluded(h) => key < *h,
                };
                if !inside {
                    self.leaf = NIL;
                    return None;
                }
                self.pos += 1;
                return Some((key, node.values[self.pos - 1]));
            }
            match self.upper {
                Some(next) => {
                    (self.leaf, self.pos, self.upper) = self.tree.seek(Bound::Included(&next));
                }
                None => self.leaf = NIL,
            }
        }
    }
}

/// Result of loading a tree at mount.
#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct LoadReport {
    /// Key ranges `[low, high)` whose node had no valid copy. `high` of `None`
    /// is unbounded. A lost root reports `(Key::MIN, None)`.
    pub lost: Vec<(Key, Option<Key>)>,

    /// The root had no valid copy. The tree is empty.
    pub root_lost: bool,

    /// Node reads that returned bytes.
    pub nodes_read: u64,

    /// Node copies that were unreadable, invalid, or referenced twice.
    pub invalid_copies: u64,

    /// Valid, non-duplicate addresses of nodes whose range was lost. They are
    /// not reported by [`Tree::take_freed`]; the caller decides whether to
    /// release them.
    pub lost_addrs: Vec<NodeAddr>,

    /// Entries loaded.
    pub entries: u64,
}

#[derive(Clone, Copy, Debug)]
struct Request {
    parent: u32,
    pos: usize,
    addrs: [NodeAddr; 2],
    /// Copy being tried.
    copy: usize,
    /// Copies that may not be used because the address is invalid or was
    /// already referenced.
    unusable: [bool; 2],
    /// A copy failed, so the node is rewritten after loading.
    bad: bool,
    level: u8,
    low: Key,
    high: Option<Key>,
}

/// Step-wise, I/O-free tree loader. The caller reads the addresses from
/// [`Self::next_reads`], passes the bytes to [`Self::supply`], and repeats
/// until [`Self::is_done`].
#[derive(Debug)]
pub struct Loader {
    tree: Tree,
    slot_count: u64,
    max_seq: u64,
    queue: Vec<Request>,
    inflight: HashMap<NodeAddr, Request>,
    seen: HashSet<NodeAddr>,
    report: LoadReport,
}

impl Loader {
    /// Start loading the tree of a superblock. `entries_hint` sizes memory only.
    pub fn new(
        volume_id: [u8; 16],
        slot_count: u64,
        max_seq: u64,
        root: [NodeAddr; 2],
        root_level: u8,
        entries_hint: u64,
    ) -> Self {
        let mut tree = Tree::new(volume_id);
        let hint = usize::try_from(entries_hint / (LEAF_CAPACITY as u64 / 2)).unwrap_or(0);
        tree.nodes.reserve(hint.min(1 << 24));
        let mut loader = Self {
            tree,
            slot_count,
            max_seq,
            queue: Vec::new(),
            inflight: HashMap::new(),
            seen: HashSet::new(),
            report: LoadReport::default(),
        };
        if root != NO_ADDRS {
            loader.enqueue(NIL, 0, root, root_level, Key::MIN, None);
        }
        loader
    }

    /// Addresses to read now. Empty when nothing is outstanding.
    pub fn next_reads(&mut self) -> Vec<NodeAddr> {
        self.queue
            .drain(..)
            .map(|req| {
                let addr = req.addrs[req.copy];
                self.inflight.insert(addr, req);
                addr
            })
            .collect()
    }

    /// Bytes read from `addr`, or `None` on a read error. Unknown addresses
    /// are ignored.
    pub fn supply(&mut self, addr: NodeAddr, bytes: Option<&[u8; UNIT_BYTES]>) {
        let Some(mut req) = self.inflight.remove(&addr) else {
            return;
        };
        let decoded = bytes.and_then(|bytes| {
            self.report.nodes_read += 1;
            node::decode(
                bytes,
                &Expect {
                    volume_id: self.tree.volume_id,
                    slot_count: self.slot_count,
                    addr,
                    max_seq: self.max_seq,
                    level: Some(req.level),
                    low: req.low,
                    high: req.high,
                },
            )
        });
        match decoded {
            Some(node) => self.accept(req, node),
            None => {
                self.report.invalid_copies += 1;
                req.bad = true;
                req.copy += 1;
                self.try_copy(req);
            }
        }
    }

    /// Whether every read has been supplied.
    pub fn is_done(&self) -> bool {
        self.queue.is_empty() && self.inflight.is_empty()
    }

    /// Build the tree. Nodes still outstanding are treated as lost.
    pub fn finish(mut self) -> (Tree, LoadReport) {
        let outstanding: Vec<Request> = self
            .queue
            .drain(..)
            .chain(self.inflight.drain().map(|(_, r)| r))
            .collect();
        for req in outstanding {
            self.lose(req);
        }
        let mut tree = self.tree;
        if tree.root != NIL && tree.root != LOST && !tree.prune(tree.root) {
            let root = tree.root;
            tree.free_node(root);
            tree.set_root(NIL);
        }
        if tree.root == LOST {
            tree.root = NIL;
        }
        if self.report.root_lost {
            tree.root_change += 1;
        }
        tree.fix_root();
        tree.len = tree
            .nodes
            .iter()
            .filter(|n| n.live && n.level == 0)
            .map(|n| n.keys.len() as u64)
            .sum();
        self.report.entries = tree.len;
        (tree, self.report)
    }

    fn enqueue(
        &mut self,
        parent: u32,
        pos: usize,
        addrs: [NodeAddr; 2],
        level: u8,
        low: Key,
        high: Option<Key>,
    ) {
        let copies = if level == 0 { 1 } else { 2 };
        let mut unusable = [true; 2];
        for (copy, addr) in addrs.iter().enumerate().take(copies) {
            unusable[copy] = !addr.valid_for(self.slot_count) || !self.seen.insert(*addr);
        }
        let req = Request {
            parent,
            pos,
            addrs,
            copy: 0,
            unusable,
            bad: false,
            level,
            low,
            high,
        };
        self.try_copy(req);
    }

    /// Queue the first usable copy at or after `req.copy`, or lose the node.
    fn try_copy(&mut self, mut req: Request) {
        let copies = if req.level == 0 { 1 } else { 2 };
        while req.copy < copies {
            if !req.unusable[req.copy] {
                self.queue.push(req);
                return;
            }
            self.report.invalid_copies += 1;
            req.bad = true;
            req.copy += 1;
        }
        self.lose(req);
    }

    fn lose(&mut self, req: Request) {
        self.report.lost.push((req.low, req.high));
        let copies = if req.level == 0 { 1 } else { 2 };
        for copy in 0..copies {
            if !req.unusable[copy] {
                self.report.lost_addrs.push(req.addrs[copy]);
            }
        }
        if req.parent == NIL {
            self.report.root_lost = true;
            self.tree.root = LOST;
        } else {
            self.tree.nodes[req.parent as usize].children[req.pos] = LOST;
        }
    }

    fn accept(&mut self, req: Request, decoded: DecodedNode) {
        let idx = self.tree.alloc_node(decoded.level);
        let node = &mut self.tree.nodes[idx as usize];
        let mut children = Vec::new();
        match decoded.body {
            NodeBody::Leaf(entries) => {
                for (key, value) in entries {
                    node.keys.push(key);
                    node.values.push(value);
                }
            }
            NodeBody::Inner(entries) => {
                for (key, addrs) in entries {
                    node.keys.push(key);
                    node.children.push(PENDING);
                    children.push(addrs);
                }
            }
        }
        node.dirty = false;
        node.addrs = NO_ADDRS;
        let copies = if req.level == 0 { 1 } else { 2 };
        for copy in 0..copies {
            if !req.unusable[copy] {
                node.addrs[copy] = req.addrs[copy];
            }
        }
        if req.bad {
            // Rewrite the node so both copies are good again.
            self.tree.touch(idx);
        }
        if req.parent == NIL {
            self.tree.root = idx;
        } else {
            self.tree.nodes[req.parent as usize].children[req.pos] = idx;
        }
        let node = &self.tree.nodes[idx as usize];
        let keys: Vec<Key> = if children.is_empty() {
            Vec::new()
        } else {
            node.keys.clone()
        };
        for (i, addrs) in children.into_iter().enumerate() {
            let low = if i == 0 { req.low } else { keys[i] };
            let high = keys.get(i + 1).copied().or(req.high);
            self.enqueue(idx, i, addrs, req.level - 1, low, high);
        }
    }
}

impl Tree {
    /// Remove lost and empty children below `idx`. Returns whether `idx`
    /// still has entries.
    fn prune(&mut self, idx: u32) -> bool {
        if self.nodes[idx as usize].level == 0 {
            return true;
        }
        let mut changed = false;
        let mut i = self.nodes[idx as usize].children.len();
        while i > 0 {
            i -= 1;
            let child = self.nodes[idx as usize].children[i];
            let keep = child != LOST && child != PENDING && {
                let kept = self.prune(child);
                if !kept {
                    self.free_node(child);
                }
                kept
            };
            if keep {
                changed |= self.nodes[child as usize].dirty;
            } else {
                let node = &mut self.nodes[idx as usize];
                node.keys.remove(i);
                node.children.remove(i);
                changed = true;
            }
        }
        if changed {
            self.touch(idx);
        }
        !self.nodes[idx as usize].keys.is_empty()
    }
}

#[cfg(test)]
mod tests;
