// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! CoW B+tree primitives: bulk-loading and root-anchored lookup.
//!
//! The mutator side does *bulk rewrite* on every commit: the
//! current state is held as a `BTreeMap<PageKey, LeafEntry>` in
//! memory, and a commit serializes the whole map into a fresh
//! tree, allocating new LBAs. Old pages become free after the
//! [`RootSnapshot`](super::RootSnapshot) that pinned them drops.
//!
//! This is intentionally simpler than a true page-incremental CoW
//! tree: the trade-off is a higher write amplification per commit
//! (proportional to the index size, not the batch size) for a
//! drastically simpler implementation. For per-disk index sizes
//! seen in production (low tens of MiB even on multi-TB disks)
//! the bulk approach is well within budget.
//!
//! The lookup side does *not* hold the in-memory map: it walks
//! the on-disk tree starting from the
//! [`RootSnapshot`](super::RootSnapshot) currently published via
//! [`arc_swap::ArcSwap`]. That keeps lookups honest - they
//! exercise the exact disk path a fresh process would.

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
