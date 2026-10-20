//! Disk-resident copy-on-write B+trees.
//!
//! A tree is a root address pair, a root level, and an entry count. Pages are
//! immutable once written and are loaded through a [`Source`] (the volume's
//! pager), so only the pages a caller touches are in memory. Leaves are
//! written once; inner pages are written twice (two addresses per child
//! reference) so one bad copy loses nothing.
//!
//! [`apply`] merges a sorted batch of changes into a tree and returns the
//! new root. It loads only the pages on the changed paths, rewrites them to
//! new pages, and reports the addresses it stopped referencing. Leaves are
//! handed to a [`Sink`] as soon as they are final (the commit encodes and
//! writes them at once, which bounds memory); inner pages are kept as
//! [`NewPage`]s until the caller assigns addresses.
//!
//! Pages whose copies are all unreadable or invalid are "lost": reads report
//! their key range, and a batch that touches a lost range rebuilds it from
//! the batch alone.

use std::future::Future;
use std::pin::Pin;
use std::rc::Rc;

use super::format::{Key, NodeAddr};
use super::page::{INNER_CAPACITY, Raw, TreeKind, TreePage};
use super::pager::Priority;

/// Boxed local future.
pub type LocalFuture<'a, T> = Pin<Box<dyn Future<Output = T> + 'a>>;

/// Non-root inner pages are merged with a neighbor below this many children.
const INNER_MIN: usize = INNER_CAPACITY / 4;

/// Root of one tree, as recorded in the superblock.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct TreeRoot {
    /// Root addresses. `NONE` twice for an empty tree; the second is `NONE`
    /// for a leaf root.
    pub root: [NodeAddr; 2],

    /// Root level.
    pub level: u8,

    /// Entries.
    pub count: u64,
}

impl TreeRoot {
    /// Empty tree.
    pub const EMPTY: Self = Self {
        root: [NodeAddr::NONE; 2],
        level: 0,
        count: 0,
    };

    /// Whether the tree has no pages.
    pub fn is_empty(&self) -> bool {
        self.root[0].is_none()
    }
}

/// A page request: where the page is and what its parent expects.
#[derive(Clone, Copy, Debug)]
pub struct Req {
    /// Copies of the page.
    pub addrs: [NodeAddr; 2],

    /// Tree.
    pub kind: TreeKind,

    /// Expected level.
    pub level: u8,

    /// Seq of the root set being read. The page may not be newer.
    pub max_seq: u64,

    /// Inclusive lower key bound.
    pub low: Key,

    /// Exclusive upper key bound, `None` for unbounded.
    pub high: Option<Key>,

    /// Cache priority.
    pub priority: Priority,
}

/// Where tree pages come from.
pub trait Source {
    /// Error of a failed read.
    type Error;

    /// A cached copy that fits the request, without I/O.
    fn cached(&self, req: &Req) -> Option<Rc<TreePage>>;

    /// A copy that fits the request, or `None` when no copy is valid. Fails
    /// only on I/O errors that persisted through retries.
    fn load(&self, req: Req) -> LocalFuture<'_, Result<Option<Rc<TreePage>>, Self::Error>>;

    /// Start loading several pages at once; results land in the cache.
    fn prefetch(&self, reqs: Vec<Req>) -> LocalFuture<'_, ()>;
}

/// Reference to a child page from a rebuilt inner page.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Child {
    /// Already on disk (unchanged or written by the sink).
    Addr([NodeAddr; 2]),

    /// A [`NewPage`] by index, not placed yet.
    New(usize),
}

/// Contents of a page not placed yet.
#[derive(Clone, Debug)]
pub enum Body {
    /// Leaf entries.
    Leaf(Vec<(Key, Raw)>),

    /// Inner entries: lowest key of the child and the child.
    Inner(Vec<(Key, Child)>),
}

/// A page built by [`apply`] that still needs addresses.
#[derive(Clone, Debug)]
pub struct NewPage {
    /// Level.
    pub level: u8,

    /// Entries.
    pub body: Body,
}

/// Where final leaves go.
pub trait Sink {
    /// Error of a failed write.
    type Error;

    /// Take a final leaf. Returns its address, or `None` to keep it as a
    /// [`NewPage`] for the caller to place.
    fn emit<'a>(
        &'a mut self,
        entries: &'a [(Key, Raw)],
    ) -> LocalFuture<'a, Result<Option<NodeAddr>, Self::Error>>;
}

/// A sink that keeps every leaf as a [`NewPage`].
pub struct Deferred<E>(pub std::marker::PhantomData<E>);

impl<E> Sink for Deferred<E> {
    type Error = E;

    fn emit<'a>(
        &'a mut self,
        _entries: &'a [(Key, Raw)],
    ) -> LocalFuture<'a, Result<Option<NodeAddr>, Self::Error>> {
        Box::pin(async { Ok(None) })
    }
}

/// Result of a point lookup.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Lookup {
    /// The key's value.
    Found(Raw),

    /// The key is not in the tree.
    Absent,

    /// The key lies in a lost range `[low, high)`.
    Lost(Key, Option<Key>),
}

/// The leaf that covers a key, for scans.
pub enum Seek {
    /// The leaf and the exclusive upper bound of its key range.
    Leaf(Rc<TreePage>, Option<Key>),

    /// The empty tree.
    Empty,

    /// The covering page is lost; its range is `[low, high)`.
    Lost(Key, Option<Key>),
}

fn request(root: &TreeRoot, kind: TreeKind, max_seq: u64, priority: Priority) -> Req {
    Req {
        addrs: root.root,
        kind,
        level: root.level,
        max_seq,
        low: Key::MIN,
        high: None,
        priority: if root.level == 0 {
            priority
        } else {
            Priority::Inner
        },
    }
}

/// Child request for entry `i` of inner page `page` loaded for `req`.
fn child_request(page: &TreePage, i: usize, req: &Req, priority: Priority) -> Req {
    let level = req.level - 1;
    Req {
        addrs: page.child(i),
        kind: req.kind,
        level,
        max_seq: req.max_seq,
        low: if i == 0 { req.low } else { page.key(i) },
        high: if i + 1 < page.count() {
            Some(page.key(i + 1))
        } else {
            req.high
        },
        priority: if level == 0 {
            priority
        } else {
            Priority::Inner
        },
    }
}

/// Leaf covering `key`.
pub async fn seek<S: Source>(
    source: &S,
    root: &TreeRoot,
    kind: TreeKind,
    max_seq: u64,
    key: &Key,
    priority: Priority,
) -> Result<Seek, S::Error> {
    if root.is_empty() {
        return Ok(Seek::Empty);
    }
    let mut req = request(root, kind, max_seq, priority);
    loop {
        let Some(page) = source.load(req).await? else {
            return Ok(Seek::Lost(req.low, req.high));
        };
        if req.level == 0 {
            return Ok(Seek::Leaf(page, req.high));
        }
        let i = page.child_index(key);
        req = child_request(&page, i, &req, priority);
    }
}

/// Value of `key`.
pub async fn get<S: Source>(
    source: &S,
    root: &TreeRoot,
    kind: TreeKind,
    max_seq: u64,
    key: &Key,
) -> Result<Lookup, S::Error> {
    Ok(
        match seek(source, root, kind, max_seq, key, Priority::Leaf).await? {
            Seek::Empty => Lookup::Absent,
            Seek::Lost(low, high) => Lookup::Lost(low, high),
            Seek::Leaf(page, _) => match page.search(key) {
                Ok(i) => Lookup::Found(page.raw(i)),
                Err(_) => Lookup::Absent,
            },
        },
    )
}

/// Value of `key` from cached pages only. `None` when a page on the path is
/// not cached.
pub fn try_get<S: Source>(
    source: &S,
    root: &TreeRoot,
    kind: TreeKind,
    max_seq: u64,
    key: &Key,
) -> Option<Lookup> {
    if root.is_empty() {
        return Some(Lookup::Absent);
    }
    let mut req = request(root, kind, max_seq, Priority::Leaf);
    loop {
        let page = source.cached(&req)?;
        if req.level == 0 {
            return Some(match page.search(key) {
                Ok(i) => Lookup::Found(page.raw(i)),
                Err(_) => Lookup::Absent,
            });
        }
        let i = page.child_index(key);
        req = child_request(&page, i, &req, Priority::Leaf);
    }
}

/// Every page address of a tree above the leaves, and every leaf address,
/// visiting only inner pages. Calls `visit` with each address and level.
/// Returns the lost ranges of inner pages.
pub async fn walk_inner<S: Source>(
    source: &S,
    root: &TreeRoot,
    kind: TreeKind,
    max_seq: u64,
    visit: &mut dyn FnMut(NodeAddr, u8),
) -> Result<Vec<(Key, Option<Key>)>, S::Error> {
    let mut lost = Vec::new();
    if root.is_empty() {
        return Ok(lost);
    }
    for addr in root.root.iter().filter(|a| !a.is_none()) {
        visit(*addr, root.level);
    }
    if root.level == 0 {
        return Ok(lost);
    }
    let mut stack = vec![request(root, kind, max_seq, Priority::Scan)];
    while let Some(req) = stack.pop() {
        let Some(page) = source
            .load(Req {
                priority: Priority::Scan,
                ..req
            })
            .await?
        else {
            lost.push((req.low, req.high));
            continue;
        };
        for i in 0..page.count() {
            let child = child_request(&page, i, &req, Priority::Scan);
            for addr in child.addrs.iter().filter(|a| !a.is_none()) {
                visit(*addr, child.level);
            }
            if child.level > 0 {
                stack.push(child);
            }
        }
    }
    Ok(lost)
}

/// Output of [`apply`].
#[derive(Debug, Default)]
pub struct Applied {
    /// New root reference at `level`: `None` for an empty tree.
    pub root: Option<Child>,

    /// Root level.
    pub level: u8,

    /// Pages to place: inner pages, and leaves the sink kept.
    pub pages: Vec<NewPage>,

    /// Addresses of pages the new tree no longer references.
    pub freed: Vec<NodeAddr>,

    /// Lost ranges the batch touched (rebuilt from the batch alone).
    pub lost: Vec<(Key, Option<Key>)>,
}

/// Change for one key: the resolver turns the old value into the new one.
pub type Resolve<'r, T> = dyn FnMut(&Key, Option<&Raw>, &T) -> Option<Raw> + 'r;

struct Ctx<'a, 'r, S: Source, K: Sink<Error = S::Error>, T> {
    source: &'a S,
    sink: &'a mut K,
    resolve: &'a mut Resolve<'r, T>,
    kind: TreeKind,
    out: Applied,
}

/// Merge `ops` (sorted by key, unique) into the tree at `root`.
///
/// `resolve` gets each key's old value (if any) and its change, and returns
/// the new value (or `None` to delete). `max_seq` is the seq of the root set
/// `root` belongs to. Fails only when a page read fails or the sink fails.
pub async fn apply<S, K, T>(
    source: &S,
    sink: &mut K,
    root: &TreeRoot,
    kind: TreeKind,
    max_seq: u64,
    ops: &[(Key, T)],
    resolve: &mut Resolve<'_, T>,
) -> Result<Applied, S::Error>
where
    S: Source,
    K: Sink<Error = S::Error>,
{
    let mut ctx = Ctx {
        source,
        sink,
        resolve,
        kind,
        out: Applied::default(),
    };
    if ops.is_empty() {
        ctx.out.root = (!root.is_empty()).then_some(Child::Addr(root.root));
        ctx.out.level = root.level;
        return Ok(ctx.out);
    }
    let (mut list, mut level) = if root.is_empty() {
        let entries = ctx.merge(None, ops);
        (ctx.emit_leaves(entries).await?, 0)
    } else {
        let req = request(root, kind, max_seq, Priority::Leaf);
        if root.level == 0 {
            let entries = ctx.merge_page(&req, ops).await?;
            (ctx.emit_leaves(entries).await?, 0)
        } else {
            (ctx.rebuild(req, ops).await?, root.level - 1)
        }
    };
    if list.is_empty() {
        ctx.out.root = None;
        ctx.out.level = 0;
        return Ok(ctx.out);
    }
    while list.len() > 1 {
        list = ctx.chunk_inner(list, level + 1);
        level += 1;
    }
    let mut child = list[0].1;
    // Collapse single-child inner roots.
    while level > 0 {
        let Child::New(index) = child else { break };
        let Body::Inner(entries) = &ctx.out.pages[index].body else {
            break;
        };
        if entries.len() != 1 {
            break;
        }
        child = entries[0].1;
        level -= 1;
    }
    ctx.out.root = Some(child);
    ctx.out.level = level;
    Ok(ctx.out)
}

impl<S: Source, K: Sink<Error = S::Error>, T> Ctx<'_, '_, S, K, T> {
    fn free(&mut self, addrs: [NodeAddr; 2]) {
        self.out
            .freed
            .extend(addrs.into_iter().filter(|a| !a.is_none()));
    }

    /// Merge a leaf's entries (or none) with ops.
    fn merge(&mut self, page: Option<&TreePage>, ops: &[(Key, T)]) -> Vec<(Key, Raw)> {
        let count = page.map_or(0, |p| p.count());
        let mut out = Vec::with_capacity(count + ops.len());
        let mut i = 0;
        for (key, op) in ops {
            let mut old = None;
            if let Some(page) = page {
                while i < count && page.key(i) < *key {
                    out.push((page.key(i), page.raw(i)));
                    i += 1;
                }
                if i < count && page.key(i) == *key {
                    old = Some(page.raw(i));
                    i += 1;
                }
            }
            if let Some(new) = (self.resolve)(key, old.as_ref(), op) {
                out.push((*key, new));
            }
        }
        if let Some(page) = page {
            while i < count {
                out.push((page.key(i), page.raw(i)));
                i += 1;
            }
        }
        out
    }

    /// Load a leaf (or treat it as empty when lost) and merge ops into it.
    async fn merge_page(
        &mut self,
        req: &Req,
        ops: &[(Key, T)],
    ) -> Result<Vec<(Key, Raw)>, S::Error> {
        match self.source.load(*req).await? {
            Some(page) => {
                self.free(req.addrs);
                Ok(self.merge(Some(&page), ops))
            }
            None => {
                self.out.lost.push((req.low, req.high));
                Ok(self.merge(None, ops))
            }
        }
    }

    /// Split `entries` evenly into leaves and hand them to the sink.
    async fn emit_leaves(
        &mut self,
        entries: Vec<(Key, Raw)>,
    ) -> Result<Vec<(Key, Child)>, S::Error> {
        let mut list = Vec::new();
        for chunk in even_chunks(entries.len(), self.kind.leaf_capacity()) {
            let part = &entries[chunk];
            let child = match self.sink.emit(part).await? {
                Some(addr) => Child::Addr([addr, NodeAddr::NONE]),
                None => {
                    self.out.pages.push(NewPage {
                        level: 0,
                        body: Body::Leaf(part.to_vec()),
                    });
                    Child::New(self.out.pages.len() - 1)
                }
            };
            list.push((part[0].0, child));
        }
        Ok(list)
    }

    /// Split a child list evenly into new inner pages at `level`.
    fn chunk_inner(&mut self, list: Vec<(Key, Child)>, level: u8) -> Vec<(Key, Child)> {
        let mut out = Vec::new();
        for chunk in even_chunks(list.len(), INNER_CAPACITY) {
            let part = list[chunk].to_vec();
            let key = part[0].0;
            self.out.pages.push(NewPage {
                level,
                body: Body::Inner(part),
            });
            out.push((key, Child::New(self.out.pages.len() - 1)));
        }
        out
    }

    /// New child list of the inner page at `req` after applying `ops`.
    fn rebuild<'s>(
        &'s mut self,
        req: Req,
        ops: &'s [(Key, T)],
    ) -> LocalFuture<'s, Result<Vec<(Key, Child)>, S::Error>> {
        Box::pin(async move {
            let Some(page) = self.source.load(req).await? else {
                // Lost: rebuild the range from the batch alone.
                self.out.lost.push((req.low, req.high));
                let entries = self.merge(None, ops);
                let mut list = self.emit_leaves(entries).await?;
                let mut level = 0;
                while level + 1 < req.level {
                    level += 1;
                    list = self.chunk_inner(list, level);
                }
                return Ok(list);
            };
            self.free(req.addrs);
            let count = page.count();
            // Ops per child.
            let mut spans = Vec::new();
            let mut start = 0;
            while start < ops.len() {
                let i = page.child_index(&ops[start].0);
                let high = (i + 1 < count).then(|| page.key(i + 1));
                let end =
                    start + ops[start..].partition_point(|(key, _)| high.is_none_or(|h| *key < h));
                spans.push((i, start..end));
                start = end;
            }
            let child_level = req.level - 1;
            if spans.len() > 1 {
                let reqs = spans
                    .iter()
                    .map(|(i, _)| child_request(&page, *i, &req, Priority::Leaf))
                    .collect();
                self.source.prefetch(reqs).await;
            }
            let mut list: Vec<(Key, Child)> = Vec::new();
            let mut next = 0;
            let mut span = 0;
            while next < count {
                if span >= spans.len() || spans[span].0 != next {
                    list.push((page.key(next), Child::Addr(page.child(next))));
                    next += 1;
                    continue;
                }
                // A run of consecutive changed children.
                let run_start = next;
                let changed = |i: usize| spans.iter().any(|(s, _)| *s == i);
                let mut leaves: Vec<(Key, Raw)> = Vec::new();
                let mut children: Vec<(Key, Child)> = Vec::new();
                while span < spans.len() && spans[span].0 == next {
                    let child = child_request(&page, next, &req, Priority::Leaf);
                    let range = spans[span].1.clone();
                    if child_level == 0 {
                        let merged = self.merge_page(&child, &ops[range]).await?;
                        leaves.extend(merged);
                    } else {
                        let rebuilt = self.rebuild(child, &ops[range]).await?;
                        children.extend(rebuilt);
                    }
                    next += 1;
                    span += 1;
                }
                let len = if child_level == 0 {
                    leaves.len()
                } else {
                    children.len()
                };
                let min = if child_level == 0 {
                    self.kind.leaf_capacity() / 4
                } else {
                    INNER_MIN
                };
                if len > 0 && len < min && count > 1 {
                    // Pull in an unchanged neighbor: the next child, else
                    // the previous one (the last entry of `list`).
                    let neighbor = if next < count && !changed(next) {
                        next += 1;
                        Some((next - 1, false))
                    } else if run_start > 0 && !changed(run_start - 1) {
                        list.pop();
                        Some((run_start - 1, true))
                    } else {
                        None
                    };
                    if let Some((i, before)) = neighbor {
                        let child = child_request(&page, i, &req, Priority::Leaf);
                        match self.source.load(child).await? {
                            Some(neighbor) => {
                                self.free(child.addrs);
                                let n = neighbor.count();
                                if child_level == 0 {
                                    let extra: Vec<_> = (0..n)
                                        .map(|j| (neighbor.key(j), neighbor.raw(j)))
                                        .collect();
                                    if before {
                                        leaves.splice(0..0, extra);
                                    } else {
                                        leaves.extend(extra);
                                    }
                                } else {
                                    let extra: Vec<_> = (0..n)
                                        .map(|j| (neighbor.key(j), Child::Addr(neighbor.child(j))))
                                        .collect();
                                    if before {
                                        children.splice(0..0, extra);
                                    } else {
                                        children.extend(extra);
                                    }
                                }
                            }
                            None => {
                                // Keep the unreadable neighbor as it is.
                                let entry = (page.key(i), Child::Addr(page.child(i)));
                                if before {
                                    list.push(entry);
                                } else {
                                    // Re-add after the run below.
                                    next -= 1;
                                }
                            }
                        }
                    }
                }
                let rebuilt = if child_level == 0 {
                    self.emit_leaves(leaves).await?
                } else {
                    self.chunk_inner(children, child_level)
                };
                list.extend(rebuilt);
            }
            Ok(list)
        })
    }
}

/// Pages of `applied` reachable from its root, children before parents.
pub fn placement_order(applied: &Applied) -> Vec<usize> {
    let mut order = Vec::new();
    let Some(Child::New(root)) = applied.root else {
        return order;
    };
    // Iterative post-order.
    let mut stack = vec![(root, false)];
    while let Some((index, expanded)) = stack.pop() {
        if expanded {
            order.push(index);
            continue;
        }
        stack.push((index, true));
        if let Body::Inner(entries) = &applied.pages[index].body {
            for (_, child) in entries.iter().rev() {
                if let Child::New(child) = child {
                    stack.push((*child, false));
                }
            }
        }
    }
    order
}

/// Addresses needed to place the pages in `order`: one per leaf, two per
/// inner page.
pub fn addresses_needed(applied: &Applied, order: &[usize]) -> usize {
    order
        .iter()
        .map(|&i| if applied.pages[i].level == 0 { 1 } else { 2 })
        .sum()
}

/// Copies of a placed child.
pub fn resolve_child(child: &Child, placed: &[[NodeAddr; 2]]) -> [NodeAddr; 2] {
    match child {
        Child::Addr(addrs) => *addrs,
        Child::New(index) => placed[*index],
    }
}

/// Encode a placed page at `addr` into `buf`. `placed` holds the addresses
/// of every page placed before it.
pub fn encode_new(
    buf: &mut [u8],
    page: &NewPage,
    placed: &[[NodeAddr; 2]],
    kind: TreeKind,
    volume_id: &[u8; 16],
    seq: u64,
    addr: NodeAddr,
) {
    match &page.body {
        Body::Leaf(entries) => super::page::encode_leaf(
            buf,
            kind,
            volume_id,
            seq,
            addr,
            entries.iter().map(|(k, v)| (k, v)),
        ),
        Body::Inner(entries) => {
            let entries: Vec<(Key, [NodeAddr; 2])> = entries
                .iter()
                .map(|(key, child)| (*key, resolve_child(child, placed)))
                .collect();
            super::page::encode_inner(buf, kind, volume_id, seq, addr, page.level, &entries);
        }
    }
}

/// Ranges that split `len` items into the fewest chunks of at most `cap`,
/// with sizes differing by at most one.
fn even_chunks(len: usize, cap: usize) -> impl Iterator<Item = std::ops::Range<usize>> {
    let chunks = len.div_ceil(cap);
    (0..chunks).map(move |i| (i * len / chunks)..((i + 1) * len / chunks))
}

#[cfg(test)]
#[path = "btree_tests.rs"]
mod tests;
