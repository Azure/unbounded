// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! CoW B+tree primitives: bulk-loading, path-copy commits, and
//! root-anchored lookup.
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
//! The lookup side does *not* hold the in-memory map: it walks
//! the on-disk tree starting from the
//! [`RootSnapshot`](super::RootSnapshot) currently published via
//! [`arc_swap::ArcSwap`]. That keeps lookups honest - they
//! exercise the exact disk path a fresh process would.

use std::future::Future;
use std::pin::Pin;
use std::rc::Rc;
use std::sync::Arc;

use crate::storage::alloc::Allocator;
use crate::storage::blockdev::{BlockDevice, ScratchPool};
use crate::storage::btree::page::{self, Decoded, LeafEntry, max_internal_keys, max_leaf_entries};
use crate::storage::types::{Error, Lba, PageKey};

/// Cycle guard for DFS traversals over the on-disk tree:
/// checksum-valid but structurally corrupt pages could otherwise
/// loop forever. Sized well above any plausible real tree depth
/// times fanout.
const MAX_TRAVERSAL_NODES: u64 = 1 << 24;

/// Walk the tree rooted at `root_lba` for `key`. Returns `None`
/// for any miss, including structural corruption / checksum
/// mismatches (the design's silent-miss policy).
pub async fn lookup<B: BlockDevice>(
    device: &B,
    scratch: &Rc<ScratchPool>,
    root_lba: Lba,
    key: &PageKey,
) -> Result<Option<LeafEntry>, Error> {
    if !root_lba.is_valid() {
        return Ok(None);
    }
    let mut cur = root_lba;
    let mut buf = scratch.acquire().await;
    // Bound the descent by the worst-case depth so a cycle in
    // corrupted-but-checksum-valid pages can't loop forever.
    for _ in 0..32 {
        match device.read(cur, buf.as_mut_slice()).await {
            Ok(()) => {}
            Err(_) => return Ok(None),
        }
        match page::decode(buf.as_slice()) {
            Decoded::Empty => return Ok(None),
            Decoded::Leaf { entries, .. } => {
                // Linear scan; leaves are at most ~72 entries on
                // 4 KiB pages so the binary-search win is tiny
                // and the linear version is much easier to read.
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
        let decoded = page::decode(buf.as_slice());
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
/// written page. Pages are allocated up-front (one per node) and
/// written serially; the caller treats the returned LBAs as
/// owned by the new snapshot.
pub async fn build_tree<B: BlockDevice>(
    device: &B,
    scratch: &Rc<ScratchPool>,
    allocator: &Allocator,
    txn_id: u64,
    sorted_entries: &[(PageKey, LeafEntry)],
) -> Result<(Lba, Vec<Lba>), Error> {
    let ps = device.page_size();
    let leaf_cap = max_leaf_entries(ps);
    let internal_cap = max_internal_keys(ps);
    debug_assert!(leaf_cap >= 1);
    debug_assert!(internal_cap >= 2);

    let mut allocated = Vec::new();

    if sorted_entries.is_empty() {
        let lba = allocator.alloc()?;
        allocated.push(lba);
        let page = page::encode_empty_leaf(ps, txn_id);
        write_or_unwind(device, scratch, allocator, lba, &page, &mut allocated).await?;
        return Ok((lba, allocated));
    }

    // Build leaves in order. Track (smallest_key, lba) per leaf
    // so the next internal level can be built without re-reading.
    let mut current: Vec<(PageKey, Lba)> =
        Vec::with_capacity(sorted_entries.len().div_ceil(leaf_cap));
    for chunk in sorted_entries.chunks(leaf_cap) {
        let lba = allocator.alloc()?;
        allocated.push(lba);
        let page = page::encode_leaf(ps, txn_id, chunk)?;
        write_or_unwind(device, scratch, allocator, lba, &page, &mut allocated).await?;
        current.push((chunk[0].0, lba));
    }

    while current.len() > 1 {
        let mut next: Vec<(PageKey, Lba)> =
            Vec::with_capacity(current.len().div_ceil(internal_cap));
        for chunk in current.chunks(internal_cap) {
            let keys: Vec<PageKey> = chunk.iter().map(|(k, _)| *k).collect();
            let children: Vec<Lba> = chunk.iter().map(|(_, l)| *l).collect();
            let lba = allocator.alloc()?;
            allocated.push(lba);
            let page = page::encode_internal(ps, txn_id, &keys, &children)?;
            write_or_unwind(device, scratch, allocator, lba, &page, &mut allocated).await?;
            next.push((chunk[0].0, lba));
        }
        current = next;
    }

    Ok((current[0].1, allocated))
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
    txn_id: u64,
    sorted_ops: Vec<(PageKey, Option<LeafEntry>)>,
) -> Result<PathCopyResult, Error> {
    let ps = device.page_size();
    let mut ctx = PathCopyCtx {
        device,
        scratch,
        allocator,
        txn_id,
        new_pages: Vec::new(),
        retired_pages: Vec::new(),
        leaf_cap: max_leaf_entries(ps),
        internal_cap: max_internal_keys(ps),
        page_size: ps,
    };
    debug_assert!(ctx.leaf_cap >= 1);
    debug_assert!(ctx.internal_cap >= 2);

    match apply_path_copy_inner(&mut ctx, parent_root, sorted_ops).await {
        Ok(new_root) => Ok(PathCopyResult {
            new_root,
            new_pages: ctx.new_pages,
            retired_pages: ctx.retired_pages,
        }),
        Err(e) => {
            free_all(ctx.allocator, &ctx.new_pages);
            Err(e)
        }
    }
}

async fn apply_path_copy_inner<B: BlockDevice>(
    ctx: &mut PathCopyCtx<'_, B>,
    parent_root: Lba,
    sorted_ops: Vec<(PageKey, Option<LeafEntry>)>,
) -> Result<Lba, Error> {
    // Walk down from the parent root, rewriting only the spine
    // pages on the touched paths. The recursion returns 0+
    // (smallest_key, lba) entries describing the contents of the
    // *replacement* node: 0 means the subtree collapsed away,
    // 1 means it fit in a single node, >1 means it split.
    let result = ctx.apply_node(parent_root, sorted_ops).await?;

    if result.is_empty() {
        // The tree is now empty: publish an empty leaf as root so
        // future commits have something to descend from.
        let lba = ctx.allocator.alloc()?;
        ctx.new_pages.push(lba);
        let page = page::encode_empty_leaf(ctx.page_size, ctx.txn_id);
        write_or_unwind(
            ctx.device,
            ctx.scratch,
            ctx.allocator,
            lba,
            &page,
            &mut ctx.new_pages,
        )
        .await?;
        return Ok(lba);
    }

    if result.len() == 1 {
        return Ok(result[0].1);
    }

    // Multiple top-level children: stack one or more internal
    // layers on top until a single root remains. Each layer is
    // chunked at `internal_cap`; depth grows by `log_internal_cap`
    // per power-of-fanout.
    let mut cur = result;
    while cur.len() > 1 {
        cur = ctx.build_internal_layer(cur).await?;
    }
    Ok(cur[0].1)
}

/// Scratchpad for an in-flight path-copy commit. Tracks the
/// fixed-per-commit parameters (`device`/`txn_id`/...) alongside
/// the growing `new_pages` and `retired_pages` lists. Held only
/// for the lifetime of [`apply_path_copy`].
struct PathCopyCtx<'a, B: BlockDevice> {
    device: &'a B,
    scratch: &'a Rc<ScratchPool>,
    allocator: &'a Allocator,
    txn_id: u64,
    new_pages: Vec<Lba>,
    retired_pages: Vec<Lba>,
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
    /// internal nodes is unbounded by static type.
    fn apply_node<'b>(
        &'b mut self,
        node_lba: Lba,
        ops: Vec<(PageKey, Option<LeafEntry>)>,
    ) -> Pin<Box<dyn Future<Output = Result<Vec<(PageKey, Lba)>, Error>> + 'b>>
    where
        'a: 'b,
    {
        Box::pin(async move {
            let decoded = {
                let mut buf = self.scratch.acquire().await;
                self.device.read(node_lba, buf.as_mut_slice()).await?;
                page::decode(buf.as_slice())
            };

            match decoded {
                Decoded::Leaf { entries, .. } => {
                    self.retired_pages.push(node_lba);
                    let merged = merge_leaf(entries, &ops);
                    self.write_leaves(&merged).await
                }
                Decoded::Internal { keys, children, .. } => {
                    if keys.is_empty() || keys.len() != children.len() {
                        return Err(Error::Corrupt);
                    }
                    self.retired_pages.push(node_lba);
                    self.recurse_internal(keys, children, ops).await
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
        &mut self,
        keys: Vec<PageKey>,
        children: Vec<Lba>,
        ops: Vec<(PageKey, Option<LeafEntry>)>,
    ) -> Result<Vec<(PageKey, Lba)>, Error> {
        // Partition `ops` into per-child buckets. Child `i` owns
        // keys in `[keys[i], keys[i+1])`; the lower bound on
        // child 0 is implicitly -infinity so we don't gate on
        // `keys[0]`. `ops` is sorted, so we sweep a single
        // cursor through it instead of binary-searching each
        // bucket.
        let mut new_children: Vec<(PageKey, Lba)> = Vec::new();
        let mut cursor = 0usize;
        for i in 0..keys.len() {
            let end = if i + 1 < keys.len() {
                let upper = &keys[i + 1];
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
                new_children.push((keys[i], children[i]));
            } else {
                let bucket: Vec<(PageKey, Option<LeafEntry>)> = ops[cursor..end].to_vec();
                let sub = self.apply_node(children[i], bucket).await?;
                new_children.extend(sub);
            }
            cursor = end;
        }
        debug_assert_eq!(cursor, ops.len());

        self.write_internals(new_children).await
    }

    async fn write_leaves(
        &mut self,
        entries: &[(PageKey, LeafEntry)],
    ) -> Result<Vec<(PageKey, Lba)>, Error> {
        if entries.is_empty() {
            return Ok(Vec::new());
        }
        let mut out: Vec<(PageKey, Lba)> =
            Vec::with_capacity(entries.len().div_ceil(self.leaf_cap));
        for chunk in entries.chunks(self.leaf_cap) {
            let lba = self.allocator.alloc()?;
            self.new_pages.push(lba);
            let page = page::encode_leaf(self.page_size, self.txn_id, chunk)?;
            write_or_unwind(
                self.device,
                self.scratch,
                self.allocator,
                lba,
                &page,
                &mut self.new_pages,
            )
            .await?;
            out.push((chunk[0].0, lba));
        }
        Ok(out)
    }

    async fn write_internals(
        &mut self,
        children: Vec<(PageKey, Lba)>,
    ) -> Result<Vec<(PageKey, Lba)>, Error> {
        // Collapse trivial cases: a removed subtree returns the
        // empty list to its caller; a non-split rewrite returns
        // the single child unchanged so we don't rewrite the
        // internal layer above it.
        if children.len() <= 1 {
            return Ok(children);
        }
        self.build_internal_layer(children).await
    }

    async fn build_internal_layer(
        &mut self,
        children: Vec<(PageKey, Lba)>,
    ) -> Result<Vec<(PageKey, Lba)>, Error> {
        debug_assert!(!children.is_empty());
        let mut out: Vec<(PageKey, Lba)> =
            Vec::with_capacity(children.len().div_ceil(self.internal_cap));
        for chunk in children.chunks(self.internal_cap) {
            let keys: Vec<PageKey> = chunk.iter().map(|(k, _)| *k).collect();
            let kids: Vec<Lba> = chunk.iter().map(|(_, l)| *l).collect();
            let lba = self.allocator.alloc()?;
            self.new_pages.push(lba);
            let page = page::encode_internal(self.page_size, self.txn_id, &keys, &kids)?;
            write_or_unwind(
                self.device,
                self.scratch,
                self.allocator,
                lba,
                &page,
                &mut self.new_pages,
            )
            .await?;
            out.push((chunk[0].0, lba));
        }
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

async fn write_or_unwind<B: BlockDevice>(
    device: &B,
    scratch: &Rc<ScratchPool>,
    allocator: &Allocator,
    lba: Lba,
    page: &[u8],
    allocated: &mut Vec<Lba>,
) -> Result<(), Error> {
    // Copy the encoded page into a registered scratch buffer
    // before submitting the write: io_uring's WRITE_FIXED path
    // requires the buffer to lie inside a previously registered
    // region, which heap `Vec` allocations do not.
    let mut buf = scratch.acquire().await;
    let slice = buf.as_mut_slice();
    debug_assert_eq!(slice.len(), page.len());
    slice.copy_from_slice(page);
    match device.write(lba, slice).await {
        Ok(()) => Ok(()),
        Err(e) => {
            // Roll back every LBA we've allocated in this commit
            // so a failed build doesn't leak pages.
            for &l in allocated.iter() {
                let _ = allocator.free(l);
            }
            allocated.clear();
            Err(e)
        }
    }
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
