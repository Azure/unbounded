// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! CoW B+tree primitives: bulk-loading, path-copy commits,
//! internal-node caching, and root-anchored lookup.
//!
//! The mutator side has two write modes:
//!
//! - [`build_tree`] does a *bulk rewrite* of the entire tree from
//!   a sorted entry list. It is used at open / bootstrap time
//!   when there is no parent tree to copy from, and by tests that
//!   want a clean "starting state".
//! - [`apply_path_copy`] does an *incremental* CoW commit: it
//!   reads only the spine from the parent root down to each
//!   mutated leaf, rewrites those pages with the mutations
//!   applied, and leaves every untouched subtree in place (the
//!   new tree shares those LBAs with the parent). This is the
//!   hot path for steady-state commits and lifts the per-commit
//!   write cost from `O(tree_size)` to `O(batch * depth)`.
//!
//! Old pages become free once the
//! [`RootSnapshot`](super::RootSnapshot) at the txn that retired
//! them is no longer reachable from any live snapshot - see the
//! generation-tracker documentation in `super`.
//!
//! Lookups use the immutable internal-node cache attached to each
//! published [`RootSnapshot`](super::RootSnapshot) to descend to a
//! leaf, then read that leaf from disk. This removes upper-level
//! metadata I/O without caching every leaf entry in memory.

use std::cell::RefCell;
use std::collections::HashMap;
use std::future::Future;
use std::mem::size_of;
use std::pin::Pin;
use std::rc::Rc;
use std::sync::Arc;
use std::task::{Context, Poll};

use crate::storage::alloc::Allocator;
use crate::storage::blockdev::{BlockDevice, ScratchPool};
use crate::storage::btree::page::{self, Decoded, LeafEntry, max_internal_keys, max_leaf_entries};
use crate::storage::types::{Error, Lba, PageKey};

/// Boxed future yielding one subtree's `(key, child_lba)` entries;
/// the recursion through internal nodes is unbounded by static
/// type, so `apply_node` and its joined siblings are erased here.
type NodeFut<'f> = Pin<Box<dyn Future<Output = Result<Vec<NodeRef>, Error>> + 'f>>;

/// Boxed future for one page write, erased so a commit's
/// independent page writes can be collected and joined.
type WriteFut<'f> = Pin<Box<dyn Future<Output = Result<(), Error>> + 'f>>;

/// Cycle guard for DFS traversals over the on-disk tree:
/// checksum-valid but structurally corrupt pages could otherwise
/// loop forever. Sized well above any plausible real tree depth
/// times fanout.
const MAX_TRAVERSAL_NODES: u64 = 1 << 24;

/// Immutable decoded cache of the internal B+tree rooted at one snapshot.
/// Internal subtrees are reference counted so a path-copy commit can share
/// every untouched branch with its parent snapshot.
#[derive(Default)]
pub struct InternalNodeCache {
    root: Option<Arc<CachedInternalNode>>,
}

struct CachedInternalNode {
    keys: Box<[PageKey]>,
    children: Box<[CachedChild]>,
    bytes: usize,
}

#[derive(Clone)]
struct CachedChild {
    lba: Lba,
    internal: Option<Arc<CachedInternalNode>>,
}

struct NodeRef {
    key: PageKey,
    child: CachedChild,
}

impl InternalNodeCache {
    pub fn bytes(&self) -> usize {
        self.root.as_ref().map_or(0, |root| root.bytes)
    }
}

impl CachedInternalNode {
    fn new(keys: Vec<PageKey>, children: Vec<CachedChild>) -> Arc<Self> {
        let bytes = size_of::<Self>()
            + keys.len() * size_of::<PageKey>()
            + children.len() * size_of::<CachedChild>()
            + children
                .iter()
                .filter_map(|child| child.internal.as_ref())
                .map(|child| child.bytes)
                .sum::<usize>();
        Arc::new(Self {
            keys: keys.into_boxed_slice(),
            children: children.into_boxed_slice(),
            bytes,
        })
    }
}

/// Build a cache for every internal node reachable from `root_lba`.
/// The traversal first discovers the tree height by following the
/// leftmost spine, then reads only levels above the leaves. That keeps
/// cache construction proportional to internal-node count, not leaf
/// count.
pub async fn build_internal_cache<B: BlockDevice>(
    device: &B,
    scratch: &Rc<ScratchPool>,
    root_lba: Lba,
    strict: bool,
) -> Result<Arc<InternalNodeCache>, Error> {
    let depth = internal_depth(device, scratch, root_lba, strict).await?;
    if depth == 0 {
        return Ok(Arc::new(InternalNodeCache::default()));
    }

    let mut nodes = HashMap::new();
    let mut stack = vec![(root_lba, depth)];
    let mut buf = scratch.acquire().await;
    let mut visited = 0u64;

    while let Some((node_lba, remaining_depth)) = stack.pop() {
        visited += 1;
        if visited > MAX_TRAVERSAL_NODES {
            return Err(Error::Corrupt);
        }
        match device.read(node_lba, buf.as_mut_slice()).await {
            Ok(()) => {}
            Err(e) if strict => return Err(e),
            Err(_) => continue,
        }

        match page::decode(buf.as_mut_slice()) {
            Decoded::Internal { keys, children, .. } if keys.len() == children.len() => {
                if remaining_depth > 1 {
                    for &child in &children {
                        stack.push((child, remaining_depth - 1));
                    }
                }
                nodes.insert(node_lba, (keys, children));
            }
            Decoded::Internal { .. } | Decoded::Empty | Decoded::Meta { .. } if strict => {
                return Err(Error::Corrupt);
            }
            _ => {}
        }
    }

    let root = materialize_cache(root_lba, depth, &nodes);
    Ok(Arc::new(InternalNodeCache { root }))
}

fn materialize_cache(
    lba: Lba,
    depth: usize,
    nodes: &HashMap<Lba, (Vec<PageKey>, Vec<Lba>)>,
) -> Option<Arc<CachedInternalNode>> {
    let (keys, child_lbas) = nodes.get(&lba)?;
    let mut children = Vec::with_capacity(child_lbas.len());
    for &child_lba in child_lbas {
        let internal = if depth > 1 {
            materialize_cache(child_lba, depth - 1, nodes)
        } else {
            None
        };
        children.push(CachedChild {
            lba: child_lba,
            internal,
        });
    }
    Some(CachedInternalNode::new(keys.clone(), children))
}

async fn internal_depth<B: BlockDevice>(
    device: &B,
    scratch: &Rc<ScratchPool>,
    root_lba: Lba,
    strict: bool,
) -> Result<usize, Error> {
    if !root_lba.is_valid() {
        return Ok(0);
    }

    let mut cur = root_lba;
    let mut depth = 0usize;
    let mut buf = scratch.acquire().await;
    for _ in 0..32 {
        match device.read(cur, buf.as_mut_slice()).await {
            Ok(()) => {}
            Err(e) if strict => return Err(e),
            Err(_) => return Ok(depth),
        }
        match page::decode(buf.as_mut_slice()) {
            Decoded::Internal { children, .. } if !children.is_empty() => {
                depth += 1;
                cur = children[0];
            }
            Decoded::Leaf { .. } => return Ok(depth),
            Decoded::Internal { .. } | Decoded::Empty | Decoded::Meta { .. } if strict => {
                return Err(Error::Corrupt);
            }
            _ => return Ok(depth),
        }
    }

    Err(Error::Corrupt)
}

/// Walk the tree rooted at `root_lba` for `key`, using cached
/// internal nodes when present and reading only the terminal leaf
/// in the common case. Returns `None` for any miss, including
/// structural corruption / checksum mismatches.
pub async fn lookup<B: BlockDevice>(
    device: &B,
    scratch: &Rc<ScratchPool>,
    root_lba: Lba,
    internal_cache: &InternalNodeCache,
    key: &PageKey,
) -> Result<Option<LeafEntry>, Error> {
    if !root_lba.is_valid() {
        return Ok(None);
    }
    let mut cur = root_lba;
    let mut cached = internal_cache.root.as_deref();
    let mut buf = scratch.acquire().await;
    for _ in 0..32 {
        if let Some(node) = cached {
            let idx = match node.keys.binary_search(key) {
                Ok(i) => i,
                Err(0) => return Ok(None),
                Err(i) => i - 1,
            };
            cur = node.children[idx].lba;
            cached = node.children[idx].internal.as_deref();
            continue;
        }

        match device.read(cur, buf.as_mut_slice()).await {
            Ok(()) => {}
            Err(_) => return Ok(None),
        }
        match page::decode(buf.as_mut_slice()) {
            Decoded::Empty => return Ok(None),
            Decoded::Leaf { entries, .. } => {
                for (k, v) in entries {
                    if &k == key {
                        return Ok(Some(v));
                    }
                }
                return Ok(None);
            }
            Decoded::Internal { keys, children, .. } => {
                let idx = match keys.binary_search(key) {
                    Ok(i) => i,
                    Err(0) => return Ok(None),
                    Err(i) => i - 1,
                };
                cur = children[idx];
            }
            Decoded::Meta { .. } => return Ok(None),
        }
    }
    Ok(None)
}

/// Walk every reachable leaf entry in the tree under `root_lba`.
/// Used during restart to rebuild the in-memory mirror; callers
/// hand in a visitor that records each `(key, value)`.
pub async fn for_each_leaf<B: BlockDevice>(
    device: &B,
    scratch: &Rc<ScratchPool>,
    root_lba: Lba,
    mut visit: impl FnMut(PageKey, LeafEntry, Lba),
) -> Result<(), Error> {
    walk_tree(device, scratch, root_lba, |node, decoded| {
        if let Decoded::Leaf { entries, .. } = decoded {
            for (k, v) in entries {
                visit(k, v, node);
            }
        }
    })
    .await
}

/// Collect every non-meta LBA reachable from `root_lba`. Used
/// after open to seed the snapshot's "owned pages" list.
pub async fn collect_pages<B: BlockDevice>(
    device: &B,
    scratch: &Rc<ScratchPool>,
    root_lba: Lba,
) -> Result<Vec<Lba>, Error> {
    let mut out = Vec::new();
    walk_tree(device, scratch, root_lba, |node, decoded| match decoded {
        Decoded::Leaf { .. } | Decoded::Internal { .. } => out.push(node),
        _ => {}
    })
    .await?;
    Ok(out)
}

/// DFS the on-disk tree from `root_lba`, invoking `visitor(node,
/// decoded)` for every page that reads back. Unreadable pages are
/// skipped (silent-miss policy); structural cycles are bounded by
/// [`MAX_TRAVERSAL_NODES`] and surfaced as `Error::Corrupt`.
/// Internal-node children are enqueued for descent regardless of
/// what the visitor does, so callers do not need to recurse.
async fn walk_tree<B, F>(
    device: &B,
    scratch: &Rc<ScratchPool>,
    root_lba: Lba,
    mut visitor: F,
) -> Result<(), Error>
where
    B: BlockDevice,
    F: FnMut(Lba, Decoded),
{
    if !root_lba.is_valid() {
        return Ok(());
    }
    let mut stack = vec![root_lba];
    let mut buf = scratch.acquire().await;
    let mut visited = 0u64;
    while let Some(node) = stack.pop() {
        visited += 1;
        if visited > MAX_TRAVERSAL_NODES {
            return Err(Error::Corrupt);
        }
        if device.read(node, buf.as_mut_slice()).await.is_err() {
            continue;
        }
        let decoded = page::decode(buf.as_mut_slice());
        if let Decoded::Internal { ref children, .. } = decoded {
            for &c in children {
                stack.push(c);
            }
        }
        visitor(node, decoded);
    }
    Ok(())
}

/// Bulk-load `sorted_entries` into a fresh tree under `device`.
/// Returns `(root_lba, allocated_lbas)`. `txn_id` stamps every
/// written page. Each level's pages are allocated up-front and
/// their writes submitted concurrently then joined, so a level's
/// I/O overlaps up to the scratch-pool depth; levels are still
/// ordered bottom-up because a parent page needs its children's
/// LBAs (known at alloc time). The caller treats the returned
/// LBAs as owned by the new snapshot. On any failure every page
/// allocated so far is freed before the error surfaces.
pub async fn build_tree<B: BlockDevice>(
    device: &B,
    scratch: &Rc<ScratchPool>,
    allocator: &Allocator,
    txn_id: u64,
    sorted_entries: &[(PageKey, LeafEntry)],
) -> Result<(Lba, Vec<Lba>), Error> {
    let mut allocated = Vec::new();
    match build_tree_inner(
        device,
        scratch,
        allocator,
        txn_id,
        sorted_entries,
        &mut allocated,
    )
    .await
    {
        Ok(root) => Ok((root, allocated)),
        Err(e) => {
            free_all(allocator, &allocated);
            Err(e)
        }
    }
}

async fn build_tree_inner<B: BlockDevice>(
    device: &B,
    scratch: &Rc<ScratchPool>,
    allocator: &Allocator,
    txn_id: u64,
    sorted_entries: &[(PageKey, LeafEntry)],
    allocated: &mut Vec<Lba>,
) -> Result<Lba, Error> {
    let ps = device.page_size();
    let leaf_cap = max_leaf_entries(ps);
    let internal_cap = max_internal_keys(ps);
    debug_assert!(leaf_cap >= 1);
    debug_assert!(internal_cap >= 2);

    if sorted_entries.is_empty() {
        let lba = allocator.alloc()?;
        allocated.push(lba);
        let page = page::encode_empty_leaf(ps, txn_id);
        write_page(device, scratch, lba, &page).await?;
        return Ok(lba);
    }

    // Build leaves in order. Track (smallest_key, lba) per leaf
    // so the next internal level can be built without re-reading.
    // All leaf LBAs are allocated and encoded first, then their
    // writes are issued concurrently and joined.
    let mut current: Vec<(PageKey, Lba)> =
        Vec::with_capacity(sorted_entries.len().div_ceil(leaf_cap));
    let mut pages: Vec<(Lba, Vec<u8>)> = Vec::with_capacity(current.capacity());
    for chunk in sorted_entries.chunks(leaf_cap) {
        let lba = allocator.alloc()?;
        allocated.push(lba);
        let page = page::encode_leaf(ps, txn_id, chunk)?;
        current.push((chunk[0].0, lba));
        pages.push((lba, page));
    }
    write_pages_concurrent(device, scratch, &pages).await?;

    while current.len() > 1 {
        let mut next: Vec<(PageKey, Lba)> =
            Vec::with_capacity(current.len().div_ceil(internal_cap));
        let mut pages: Vec<(Lba, Vec<u8>)> = Vec::with_capacity(next.capacity());
        for chunk in current.chunks(internal_cap) {
            let keys: Vec<PageKey> = chunk.iter().map(|(k, _)| *k).collect();
            let children: Vec<Lba> = chunk.iter().map(|(_, l)| *l).collect();
            let lba = allocator.alloc()?;
            allocated.push(lba);
            let page = page::encode_internal(ps, txn_id, &keys, &children)?;
            next.push((chunk[0].0, lba));
            pages.push((lba, page));
        }
        write_pages_concurrent(device, scratch, &pages).await?;
        current = next;
    }

    Ok(current[0].1)
}

/// Result of an [`apply_path_copy`] commit.
///
/// - `new_root` is the LBA of the freshly published root.
/// - `new_pages` lists every page allocated for this commit. The
///   caller owns these as part of the new snapshot.
/// - `retired_pages` lists every old-tree page that was rewritten
///   on the touched spine. These belonged to the parent snapshot
///   and become free once no live snapshot can still reach them.
pub struct PathCopyResult {
    pub new_root: Lba,
    pub new_pages: Vec<Lba>,
    pub retired_pages: Vec<Lba>,
    pub internal_cache: Arc<InternalNodeCache>,
}

/// Apply `sorted_ops` to the tree rooted at `parent_root` using
/// path-copy CoW. `sorted_ops` must be sorted by key with at most
/// one entry per key; `Some(value)` is an insert/overwrite,
/// `None` is a delete. Returns the new root plus the allocated
/// and retired LBA lists.
///
/// On any failure mid-commit every page allocated during this
/// call is freed before the error surfaces, so the parent tree
/// remains the latest durable state and the allocator does not
/// leak.
pub async fn apply_path_copy<B: BlockDevice>(
    device: &B,
    scratch: &Rc<ScratchPool>,
    allocator: &Allocator,
    parent_root: Lba,
    parent_cache: &InternalNodeCache,
    txn_id: u64,
    sorted_ops: Vec<(PageKey, Option<LeafEntry>)>,
) -> Result<PathCopyResult, Error> {
    let ps = device.page_size();
    let ctx = PathCopyCtx {
        device,
        scratch,
        allocator,
        txn_id,
        new_pages: RefCell::new(Vec::new()),
        retired_pages: RefCell::new(Vec::new()),
        leaf_cap: max_leaf_entries(ps),
        internal_cap: max_internal_keys(ps),
        page_size: ps,
    };
    debug_assert!(ctx.leaf_cap >= 1);
    debug_assert!(ctx.internal_cap >= 2);

    match apply_path_copy_inner(&ctx, parent_root, parent_cache, sorted_ops).await {
        Ok(root) => Ok(PathCopyResult {
            new_root: root.child.lba,
            new_pages: ctx.new_pages.into_inner(),
            retired_pages: ctx.retired_pages.into_inner(),
            internal_cache: Arc::new(InternalNodeCache {
                root: root.child.internal,
            }),
        }),
        Err(e) => {
            free_all(ctx.allocator, &ctx.new_pages.borrow());
            Err(e)
        }
    }
}

async fn apply_path_copy_inner<B: BlockDevice>(
    ctx: &PathCopyCtx<'_, B>,
    parent_root: Lba,
    parent_cache: &InternalNodeCache,
    sorted_ops: Vec<(PageKey, Option<LeafEntry>)>,
) -> Result<NodeRef, Error> {
    // Walk down from the parent root, rewriting only the spine
    // pages on the touched paths. The recursion returns 0+
    // (smallest_key, lba) entries describing the contents of the
    // *replacement* node: 0 means the subtree collapsed away,
    // 1 means it fit in a single node, >1 means it split.
    let result = ctx
        .apply_node(parent_root, parent_cache.root.clone(), sorted_ops)
        .await?;

    if result.is_empty() {
        // The tree is now empty: publish an empty leaf as root so
        // future commits have something to descend from.
        let lba = ctx.allocator.alloc()?;
        ctx.new_pages.borrow_mut().push(lba);
        let page = page::encode_empty_leaf(ctx.page_size, ctx.txn_id);
        write_page(ctx.device, ctx.scratch, lba, &page).await?;
        return Ok(NodeRef {
            key: PageKey::new([0; 32], 0),
            child: CachedChild {
                lba,
                internal: None,
            },
        });
    }

    if result.len() == 1 {
        return Ok(result.into_iter().next().expect("one root"));
    }

    // Multiple top-level children: stack one or more internal
    // layers on top until a single root remains. Each layer is
    // chunked at `internal_cap`; depth grows by `log_internal_cap`
    // per power-of-fanout.
    let mut cur = result;
    while cur.len() > 1 {
        cur = ctx.build_internal_layer(cur).await?;
    }
    Ok(cur.into_iter().next().expect("one root"))
}

/// Scratchpad for an in-flight path-copy commit. Tracks the
/// fixed-per-commit parameters (`device`/`txn_id`/...) alongside
/// the growing `new_pages` and `retired_pages` lists. Held only
/// for the lifetime of [`apply_path_copy`].
///
/// `new_pages` and `retired_pages` are behind [`RefCell`] so the
/// commit methods can take `&self` rather than `&mut self`. That
/// is what lets sibling subtrees in [`Self::recurse_internal`] be
/// applied concurrently (multiple shared borrows of the ctx,
/// joined together). The interior-mutability borrows are only
/// ever held across synchronous pushes, never across an `.await`,
/// so on the single-threaded executor they cannot overlap.
struct PathCopyCtx<'a, B: BlockDevice> {
    device: &'a B,
    scratch: &'a Rc<ScratchPool>,
    allocator: &'a Allocator,
    txn_id: u64,
    new_pages: RefCell<Vec<Lba>>,
    retired_pages: RefCell<Vec<Lba>>,
    leaf_cap: usize,
    internal_cap: usize,
    page_size: usize,
}

impl<'a, B: BlockDevice> PathCopyCtx<'a, B> {
    /// Recursively apply `ops` to the subtree rooted at
    /// `node_lba`. Returns the list of `(smallest_key, lba)`
    /// pairs describing the contents of the replacement
    /// subtree: empty if it collapsed, length one if it fit in
    /// a single node, longer if it split horizontally.
    ///
    /// Returns a boxed future because the recursion through
    /// internal nodes is unbounded by static type. Takes `&self`
    /// so siblings can be driven concurrently.
    fn apply_node<'b>(
        &'b self,
        node_lba: Lba,
        cached: Option<Arc<CachedInternalNode>>,
        ops: Vec<(PageKey, Option<LeafEntry>)>,
    ) -> NodeFut<'b>
    where
        'a: 'b,
    {
        Box::pin(async move {
            if let Some(node) = cached {
                self.retired_pages.borrow_mut().push(node_lba);
                return self.recurse_internal(&node, ops).await;
            }

            let decoded = {
                let mut buf = self.scratch.acquire().await;
                self.device.read(node_lba, buf.as_mut_slice()).await?;
                page::decode(buf.as_mut_slice())
            };

            match decoded {
                Decoded::Leaf { entries, .. } => {
                    self.retired_pages.borrow_mut().push(node_lba);
                    let merged = merge_leaf(entries, &ops);
                    self.write_leaves(&merged).await
                }
                Decoded::Internal { keys, children, .. } => {
                    if keys.is_empty() || keys.len() != children.len() {
                        return Err(Error::Corrupt);
                    }
                    self.retired_pages.borrow_mut().push(node_lba);
                    let node = CachedInternalNode::new(
                        keys,
                        children
                            .into_iter()
                            .map(|lba| CachedChild {
                                lba,
                                internal: None,
                            })
                            .collect(),
                    );
                    self.recurse_internal(&node, ops).await
                }
                // Empty here means the page failed to decode:
                // checksum mismatch, unknown page type, or a
                // structurally invalid leaf/internal page. The
                // encoder never produces a `PAGE_TYPE_EMPTY` page
                // (an "empty tree" is encoded as a leaf with zero
                // entries via `encode_empty_leaf`), so an Empty
                // result during a path-copy descent is always a
                // corruption signal. Treating it as a freshly
                // empty subtree (the previous behavior) silently
                // dropped every key reachable through this node
                // from the new tree while the engine's LRU and
                // reverse map kept pointing at their data LBAs,
                // breaking the resident-vs-btree invariant in
                // `tests/storage/tests.rs`. Surface it as a
                // corrupt-tree error so the commit aborts and
                // the writer cleans up its allocated LBA; a
                // retry will see the (uncorrupted) page on a
                // fresh read.
                Decoded::Empty | Decoded::Meta { .. } => Err(Error::Corrupt),
            }
        })
    }

    async fn recurse_internal(
        &self,
        node: &CachedInternalNode,
        ops: Vec<(PageKey, Option<LeafEntry>)>,
    ) -> Result<Vec<NodeRef>, Error> {
        // Partition `ops` into per-child buckets. Child `i` owns
        // keys in `[keys[i], keys[i+1])`; the lower bound on
        // child 0 is implicitly -infinity so we don't gate on
        // `keys[0]`. `ops` is sorted, so we sweep a single
        // cursor through it instead of binary-searching each
        // bucket.
        //
        // Untouched children are reused wholesale (no I/O).
        // Touched children are independent subtrees with no data
        // dependency between them, so their `apply_node` futures
        // are launched together and joined; the per-position
        // results are stitched back in key order afterward.
        let mut slots: Vec<Option<Vec<NodeRef>>> = (0..node.keys.len()).map(|_| None).collect();
        let mut futs: Vec<NodeFut<'_>> = Vec::new();
        let mut fut_positions: Vec<usize> = Vec::new();
        let mut cursor = 0usize;
        for i in 0..node.keys.len() {
            let end = if i + 1 < node.keys.len() {
                let upper = &node.keys[i + 1];
                let mut e = cursor;
                while e < ops.len() && &ops[e].0 < upper {
                    e += 1;
                }
                e
            } else {
                ops.len()
            };
            if end == cursor {
                // No mutations land in this child: reuse its
                // page wholesale. The smallest reachable key
                // hasn't changed, so we can carry `keys[i]`
                // forward unchanged.
                slots[i] = Some(vec![NodeRef {
                    key: node.keys[i],
                    child: node.children[i].clone(),
                }]);
            } else {
                let bucket: Vec<(PageKey, Option<LeafEntry>)> = ops[cursor..end].to_vec();
                futs.push(self.apply_node(
                    node.children[i].lba,
                    node.children[i].internal.clone(),
                    bucket,
                ));
                fut_positions.push(i);
            }
            cursor = end;
        }
        debug_assert_eq!(cursor, ops.len());

        // Drive all touched-child subtrees concurrently. `join_all`
        // waits for every future to finish (so no write is left
        // dangling on error) and preserves submission order.
        let results = join_all(futs).await;
        for (pos, res) in fut_positions.into_iter().zip(results) {
            slots[pos] = Some(res?);
        }

        let mut new_children: Vec<NodeRef> = Vec::new();
        for slot in slots {
            new_children.extend(slot.expect("every slot filled"));
        }

        self.write_internals(new_children).await
    }

    async fn write_leaves(&self, entries: &[(PageKey, LeafEntry)]) -> Result<Vec<NodeRef>, Error> {
        if entries.is_empty() {
            return Ok(Vec::new());
        }
        let mut out: Vec<NodeRef> = Vec::with_capacity(entries.len().div_ceil(self.leaf_cap));
        let mut pages: Vec<(Lba, Vec<u8>)> = Vec::with_capacity(out.capacity());
        for chunk in entries.chunks(self.leaf_cap) {
            let lba = self.allocator.alloc()?;
            self.new_pages.borrow_mut().push(lba);
            let page = page::encode_leaf(self.page_size, self.txn_id, chunk)?;
            out.push(NodeRef {
                key: chunk[0].0,
                child: CachedChild {
                    lba,
                    internal: None,
                },
            });
            pages.push((lba, page));
        }
        write_pages_concurrent(self.device, self.scratch, &pages).await?;
        Ok(out)
    }

    async fn write_internals(&self, children: Vec<NodeRef>) -> Result<Vec<NodeRef>, Error> {
        // Collapse trivial cases: a removed subtree returns the
        // empty list to its caller; a non-split rewrite returns
        // the single child unchanged so we don't rewrite the
        // internal layer above it.
        if children.len() <= 1 {
            return Ok(children);
        }
        self.build_internal_layer(children).await
    }

    async fn build_internal_layer(&self, children: Vec<NodeRef>) -> Result<Vec<NodeRef>, Error> {
        debug_assert!(!children.is_empty());
        let mut out: Vec<NodeRef> = Vec::with_capacity(children.len().div_ceil(self.internal_cap));
        let mut pages: Vec<(Lba, Vec<u8>)> = Vec::with_capacity(out.capacity());
        for chunk in children.chunks(self.internal_cap) {
            let keys: Vec<PageKey> = chunk.iter().map(|node| node.key).collect();
            let kids: Vec<Lba> = chunk.iter().map(|node| node.child.lba).collect();
            let lba = self.allocator.alloc()?;
            self.new_pages.borrow_mut().push(lba);
            let page = page::encode_internal(self.page_size, self.txn_id, &keys, &kids)?;
            let internal = CachedInternalNode::new(
                keys.clone(),
                chunk.iter().map(|node| node.child.clone()).collect(),
            );
            out.push(NodeRef {
                key: chunk[0].key,
                child: CachedChild {
                    lba,
                    internal: Some(internal),
                },
            });
            pages.push((lba, page));
        }
        write_pages_concurrent(self.device, self.scratch, &pages).await?;
        Ok(out)
    }
}

/// Two-pointer merge of an existing sorted leaf-entry list with
/// a sorted operation list. On a key collision the operation
/// wins: `Some(v)` overwrites, `None` deletes. Both inputs are
/// sorted ascending and contain at most one mutation per key.
fn merge_leaf(
    entries: Vec<(PageKey, LeafEntry)>,
    ops: &[(PageKey, Option<LeafEntry>)],
) -> Vec<(PageKey, LeafEntry)> {
    let mut out: Vec<(PageKey, LeafEntry)> = Vec::with_capacity(entries.len() + ops.len());
    let mut i = 0usize;
    let mut j = 0usize;
    while i < entries.len() && j < ops.len() {
        let (ek, ev) = &entries[i];
        let (ok, ov) = &ops[j];
        if ek < ok {
            out.push((*ek, *ev));
            i += 1;
        } else if ek > ok {
            if let Some(v) = ov {
                out.push((*ok, *v));
            }
            j += 1;
        } else {
            if let Some(v) = ov {
                out.push((*ok, *v));
            }
            i += 1;
            j += 1;
        }
    }
    while i < entries.len() {
        let (ek, ev) = &entries[i];
        out.push((*ek, *ev));
        i += 1;
    }
    while j < ops.len() {
        let (ok, ov) = &ops[j];
        if let Some(v) = ov {
            out.push((*ok, *v));
        }
        j += 1;
    }
    out
}

/// Write a single encoded `page` to `lba`.
///
/// The encoded bytes are first copied into a registered scratch
/// buffer because io_uring's `WRITE_FIXED` path requires the
/// source buffer to lie inside a previously registered region,
/// which a heap `Vec` does not. The lease is held only for this
/// one write, so each concurrent [`write_pages_concurrent`]
/// submission consumes exactly one pool buffer and releases it on
/// completion (no buffer is ever held across a second acquire,
/// which keeps the pool deadlock-free under oversubscription).
///
/// This does not free anything on failure: allocation cleanup is
/// centralized at the commit boundary ([`apply_path_copy`] frees
/// every page in `new_pages`; [`build_tree`] frees its
/// `allocated` list), which is correct because every LBA is
/// recorded before its write is issued.
async fn write_page<B: BlockDevice>(
    device: &B,
    scratch: &Rc<ScratchPool>,
    lba: Lba,
    page: &[u8],
) -> Result<(), Error> {
    let mut buf = scratch.acquire().await;
    let slice = buf.as_mut_slice();
    debug_assert_eq!(slice.len(), page.len());
    slice.copy_from_slice(page);
    device.write(lba, slice).await
}

/// Submit every `(lba, encoded_page)` write concurrently and wait
/// for all of them to complete, returning the first error (if
/// any) once every submission has settled. The writes within one
/// level of a commit have no data dependency on each other (a
/// parent only needs its children's LBAs, fixed at alloc time),
/// so overlapping them turns per-level latency from the sum of
/// the individual writes into roughly their max, bounded by the
/// scratch-pool depth. Waiting for all futures even on error
/// guarantees no write is still in flight when the caller starts
/// to unwind the commit's allocations.
async fn write_pages_concurrent<B: BlockDevice>(
    device: &B,
    scratch: &Rc<ScratchPool>,
    pages: &[(Lba, Vec<u8>)],
) -> Result<(), Error> {
    if pages.is_empty() {
        return Ok(());
    }
    let futs: Vec<WriteFut<'_>> = pages
        .iter()
        .map(|(lba, page)| {
            Box::pin(write_page(device, scratch, *lba, page.as_slice())) as WriteFut<'_>
        })
        .collect();
    let mut first_err: Option<Error> = None;
    for res in join_all(futs).await {
        if let Err(e) = res
            && first_err.is_none()
        {
            first_err = Some(e);
        }
    }
    match first_err {
        Some(e) => Err(e),
        None => Ok(()),
    }
}

/// Poll a set of futures concurrently to completion on the
/// single-threaded executor, returning their outputs in the same
/// order they were supplied. Modeled on the `RunAll` future in
/// `storage::local`, generalized to collect each future's output.
/// Each poll sweep advances every still-pending future; the whole
/// set resolves once the last one is ready.
async fn join_all<'f, T: Unpin>(futs: Vec<Pin<Box<dyn Future<Output = T> + 'f>>>) -> Vec<T> {
    struct JoinAll<'f, T> {
        futs: Vec<Option<Pin<Box<dyn Future<Output = T> + 'f>>>>,
        out: Vec<Option<T>>,
    }
    impl<'f, T: Unpin> Future for JoinAll<'f, T> {
        type Output = Vec<T>;
        fn poll(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<Vec<T>> {
            let me = self.get_mut();
            let mut all_done = true;
            for i in 0..me.futs.len() {
                let res = match me.futs[i].as_mut() {
                    Some(f) => f.as_mut().poll(cx),
                    None => continue,
                };
                match res {
                    Poll::Ready(v) => {
                        me.out[i] = Some(v);
                        me.futs[i] = None;
                    }
                    Poll::Pending => all_done = false,
                }
            }
            if all_done {
                Poll::Ready(
                    me.out
                        .iter_mut()
                        .map(|o| o.take().expect("ready"))
                        .collect(),
                )
            } else {
                Poll::Pending
            }
        }
    }

    let out = (0..futs.len()).map(|_| None).collect();
    let slots = futs.into_iter().map(Some).collect();
    JoinAll { futs: slots, out }.await
}

/// Free every LBA in `pages`, ignoring per-page errors. Used by
/// [`super::RootSnapshot::drop`] when its `Arc` reaches zero
/// readers.
pub fn free_all(allocator: &Allocator, pages: &[Lba]) {
    for &lba in pages {
        let _ = allocator.free(lba);
    }
}

/// Convenience: free every LBA returned by [`build_tree`] /
/// reachable from a root, wrapping the allocator in `Arc`.
#[allow(dead_code)]
pub fn free_all_arc(allocator: &Arc<Allocator>, pages: &[Lba]) {
    free_all(allocator, pages)
}
