//! Bounded cache of validated metadata pages.
//!
//! The pager holds decoded tree pages keyed by address, charged against a
//! byte budget, and evicted with CLOCK. Inner pages get more CLOCK credit than
//! leaves, so the upper levels of every tree stay resident within the budget;
//! pages faulted by scans get none, so a scan cannot flush the hot set. A page
//! is pinned while anyone holds its `Rc`, and pinned pages are never evicted
//! (the cache may then exceed its budget until the pins drop).
//!
//! The pager does no I/O itself. A reader that misses calls
//! [`Pager::begin_fault`]: the first caller for an address becomes the leader
//! and reads the page; later callers wait for the leader's result, so each
//! page has at most one read in flight. A leader dropped before finishing
//! wakes its followers, and one of them takes over.

use std::cell::RefCell;
use std::collections::{HashMap, VecDeque};
use std::future::poll_fn;
use std::hash::{BuildHasherDefault, Hasher};
use std::rc::Rc;
use std::task::{Poll, Waker};

use super::format::NodeAddr;
use super::page::TreePage;

/// Hasher for page addresses: they are already well spread `slot << 16 |
/// unit` values, so one multiply is enough.
#[derive(Default)]
pub struct AddrHasher(u64);

impl Hasher for AddrHasher {
    fn finish(&self) -> u64 {
        self.0
    }

    fn write(&mut self, bytes: &[u8]) {
        for &b in bytes {
            self.0 = (self.0 ^ u64::from(b)).wrapping_mul(0x100_0000_01b3);
        }
    }

    fn write_u64(&mut self, value: u64) {
        self.0 = (value ^ (value >> 29)).wrapping_mul(0x9e37_79b9_7f4a_7c15);
    }
}

type AddrMap<V> = HashMap<NodeAddr, V, BuildHasherDefault<AddrHasher>>;

/// Bytes charged per cached page: the page plus bookkeeping.
pub const FRAME_BYTES: usize = 4096 + 96;

/// How a page was reached, which sets its CLOCK credit.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Priority {
    /// Faulted by a scan: no credit, evicted first.
    Scan,

    /// Leaf reached by a lookup or a commit.
    Leaf,

    /// Inner page: kept longest.
    Inner,
}

impl Priority {
    fn credit(self) -> u8 {
        match self {
            Self::Scan => 0,
            Self::Leaf => 1,
            Self::Inner => 3,
        }
    }
}

/// Pager counters.
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub struct PagerStats {
    /// Lookups served from the cache.
    pub hits: u64,

    /// Lookups that needed a read.
    pub misses: u64,

    /// Pages read from disk (each fault reads once, however many waiters).
    pub reads: u64,

    /// Pages read that failed validation.
    pub invalid: u64,

    /// Pages dropped to stay within the budget.
    pub evictions: u64,

    /// Pages cached now.
    pub pages: u64,

    /// Bytes charged now.
    pub bytes: u64,

    /// Byte budget.
    pub budget: u64,
}

struct Frame {
    addr: NodeAddr,
    page: Rc<TreePage>,
    credit: u8,
    // Faulted by a scan and not used since: evicted before any CLOCK victim.
    probation: bool,
}

/// Outcome of a fault shared with followers.
#[derive(Clone)]
pub enum FaultResult {
    /// The page, validated and cached.
    Page(Rc<TreePage>),

    /// The page failed validation.
    Invalid,

    /// The leader gave up (I/O error or dropped). Followers retry.
    Retry,
}

#[derive(Default)]
struct Fault {
    result: RefCell<Option<FaultResult>>,
    waiters: RefCell<Vec<Waker>>,
}

/// What [`Pager::begin_fault`] made of the caller.
pub enum FaultRole {
    /// The page was cached by now.
    Hit(Rc<TreePage>),

    /// The caller must read the page and finish the guard.
    Leader(FaultGuard),

    /// Another caller is reading the page. Await [`Follower::wait`].
    Follower(Follower),
}

/// Held by the reader of a page. Dropping it unfinished lets a follower
/// take over.
pub struct FaultGuard {
    pager: Rc<Pager>,
    addr: NodeAddr,
    fault: Rc<Fault>,
    done: bool,
}

impl FaultGuard {
    /// Publish the result: cache a valid page and wake the followers.
    pub fn finish(mut self, page: Option<TreePage>, priority: Priority) -> FaultResult {
        self.done = true;
        let result = match page {
            Some(page) => {
                let page = Rc::new(page);
                self.pager.insert(self.addr, Rc::clone(&page), priority);
                FaultResult::Page(page)
            }
            None => {
                self.pager.state.borrow_mut().stats.invalid += 1;
                FaultResult::Invalid
            }
        };
        self.publish(result.clone());
        result
    }

    fn publish(&self, result: FaultResult) {
        self.pager.state.borrow_mut().inflight.remove(&self.addr);
        *self.fault.result.borrow_mut() = Some(result);
        for waker in self.fault.waiters.take() {
            waker.wake();
        }
    }
}

impl Drop for FaultGuard {
    fn drop(&mut self) {
        if !self.done {
            self.publish(FaultResult::Retry);
        }
    }
}

/// A caller waiting for another caller's read.
pub struct Follower(Rc<Fault>);

impl Follower {
    /// The leader's result.
    pub async fn wait(self) -> FaultResult {
        poll_fn(|cx| match &*self.0.result.borrow() {
            Some(result) => Poll::Ready(result.clone()),
            None => {
                let mut waiters = self.0.waiters.borrow_mut();
                if !waiters.iter().any(|w| w.will_wake(cx.waker())) {
                    waiters.push(cx.waker().clone());
                }
                Poll::Pending
            }
        })
        .await
    }
}

struct State {
    map: AddrMap<usize>,
    frames: Vec<Option<Frame>>,
    free: Vec<usize>,
    hand: usize,
    bytes: usize,
    budget: usize,
    inflight: AddrMap<Rc<Fault>>,
    // Scan-faulted pages in arrival order. May hold stale addresses.
    probation: VecDeque<NodeAddr>,
    stats: PagerStats,
}

/// Page cache. See the module docs.
pub struct Pager {
    state: RefCell<State>,
}

impl Pager {
    /// Empty cache with a byte budget.
    pub fn new(budget: usize) -> Rc<Self> {
        Rc::new(Self {
            state: RefCell::new(State {
                map: AddrMap::default(),
                frames: Vec::new(),
                free: Vec::new(),
                hand: 0,
                bytes: 0,
                budget,
                inflight: AddrMap::default(),
                probation: VecDeque::new(),
                stats: PagerStats::default(),
            }),
        })
    }

    /// Counters.
    pub fn stats(&self) -> PagerStats {
        let state = self.state.borrow();
        PagerStats {
            pages: state.map.len() as u64,
            bytes: state.bytes as u64,
            budget: state.budget as u64,
            ..state.stats
        }
    }

    /// Change the budget, evicting down to it where possible.
    pub fn set_budget(&self, budget: usize) {
        let mut state = self.state.borrow_mut();
        state.budget = budget;
        state.shrink(0);
    }

    /// Cached page at `addr`. A hit gives the page CLOCK credit for
    /// `priority`; scans give none.
    pub fn get(&self, addr: NodeAddr, priority: Priority) -> Option<Rc<TreePage>> {
        let mut state = self.state.borrow_mut();
        let Some(&index) = state.map.get(&addr) else {
            state.stats.misses += 1;
            return None;
        };
        state.stats.hits += 1;
        let frame = state.frames[index].as_mut().expect("mapped frame");
        frame.credit = frame.credit.max(priority.credit());
        if priority != Priority::Scan {
            frame.probation = false;
        }
        Some(Rc::clone(&frame.page))
    }

    /// Whether `addr` is cached, without counting a hit or miss.
    pub fn contains(&self, addr: NodeAddr) -> bool {
        self.state.borrow().map.contains_key(&addr)
    }

    /// Start a fault for `addr`. See the module docs.
    pub fn begin_fault(self: &Rc<Self>, addr: NodeAddr) -> FaultRole {
        let mut state = self.state.borrow_mut();
        if let Some(&index) = state.map.get(&addr) {
            let frame = state.frames[index].as_ref().expect("mapped frame");
            return FaultRole::Hit(Rc::clone(&frame.page));
        }
        if let Some(fault) = state.inflight.get(&addr) {
            return FaultRole::Follower(Follower(Rc::clone(fault)));
        }
        let fault = Rc::new(Fault::default());
        state.inflight.insert(addr, Rc::clone(&fault));
        state.stats.reads += 1;
        FaultRole::Leader(FaultGuard {
            pager: Rc::clone(self),
            addr,
            fault,
            done: false,
        })
    }

    /// Cache a page written by a commit (or read by a fault).
    pub fn insert(&self, addr: NodeAddr, page: Rc<TreePage>, priority: Priority) {
        let mut state = self.state.borrow_mut();
        if let Some(&index) = state.map.get(&addr) {
            let frame = state.frames[index].as_mut().expect("mapped frame");
            frame.page = page;
            frame.credit = priority.credit();
            frame.probation = priority == Priority::Scan;
            if frame.probation {
                state.probation.push_back(addr);
            }
            return;
        }
        state.shrink(FRAME_BYTES);
        let frame = Frame {
            addr,
            page,
            credit: priority.credit(),
            probation: priority == Priority::Scan,
        };
        if frame.probation {
            state.probation.push_back(addr);
        }
        let index = match state.free.pop() {
            Some(index) => {
                state.frames[index] = Some(frame);
                index
            }
            None => {
                state.frames.push(Some(frame));
                state.frames.len() - 1
            }
        };
        state.map.insert(addr, index);
        state.bytes += FRAME_BYTES;
    }

    /// Drop `addr` from the cache, for example when its unit is reused.
    pub fn invalidate(&self, addr: NodeAddr) {
        let mut state = self.state.borrow_mut();
        if let Some(index) = state.map.remove(&addr) {
            state.frames[index] = None;
            state.free.push(index);
            state.bytes -= FRAME_BYTES;
        }
    }
}

impl State {
    /// Evict unpinned pages until `extra` more bytes fit, or two sweeps found
    /// nothing more to evict.
    fn shrink(&mut self, extra: usize) {
        // Scan pages first, oldest first.
        let mut tries = self.probation.len();
        while self.bytes + extra > self.budget && tries > 0 {
            tries -= 1;
            let Some(addr) = self.probation.pop_front() else {
                break;
            };
            let Some(&index) = self.map.get(&addr) else {
                continue;
            };
            let frame = self.frames[index].as_ref().expect("mapped frame");
            if !frame.probation {
                continue;
            }
            if Rc::strong_count(&frame.page) > 1 {
                self.probation.push_back(addr);
                continue;
            }
            self.evict(index);
        }
        let mut steps = 2 * self.frames.len() + 1;
        while self.bytes + extra > self.budget && steps > 0 && !self.map.is_empty() {
            steps -= 1;
            if self.hand >= self.frames.len() {
                self.hand = 0;
            }
            let index = self.hand;
            self.hand += 1;
            let Some(frame) = self.frames[index].as_mut() else {
                continue;
            };
            if Rc::strong_count(&frame.page) > 1 {
                continue;
            }
            if frame.credit > 0 {
                frame.credit -= 1;
                continue;
            }
            self.evict(index);
        }
        if self.probation.len() > 2 * self.map.len() + 16 {
            let frames = &self.frames;
            let map = &self.map;
            self.probation.retain(|addr| {
                map.get(addr)
                    .is_some_and(|&i| frames[i].as_ref().is_some_and(|f| f.probation))
            });
        }
    }

    fn evict(&mut self, index: usize) {
        let frame = self.frames[index].take().expect("mapped frame");
        self.free.push(index);
        self.map.remove(&frame.addr);
        self.bytes -= FRAME_BYTES;
        self.stats.evictions += 1;
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::volume::format::{Key, UNIT_BYTES};
    use crate::volume::page::{RAW_BYTES, TreeKind, encode_leaf};

    fn page(addr: NodeAddr) -> TreePage {
        let mut bytes = Box::new([0u8; UNIT_BYTES]);
        let key = Key::new([1; 8], [2; 12], 3);
        let mut raw = [0u8; RAW_BYTES];
        raw[..32].copy_from_slice(
            &crate::volume::format::Value {
                slot: 1,
                unit: 0,
                class: 0,
                tier: crate::volume::format::Tier::Cache,
                length: 1,
                flags: 0,
                generation: 1,
                retain_until: 0,
            }
            .encode(),
        );
        encode_leaf(
            &mut bytes[..],
            TreeKind::Index,
            &[0; 16],
            1,
            addr,
            [(&key, &raw)].into_iter(),
        );
        TreePage::decode(bytes, &[0; 16], addr, 16).unwrap()
    }

    #[test]
    fn budget_is_enforced_and_inner_pages_survive_scans() {
        let pager = Pager::new(4 * FRAME_BYTES);
        let inner = NodeAddr::new(1, 2);
        pager.insert(inner, Rc::new(page(inner)), Priority::Inner);
        for unit in 3..40 {
            let addr = NodeAddr::new(1, unit);
            pager.insert(addr, Rc::new(page(addr)), Priority::Scan);
            assert!(pager.stats().bytes <= 4 * FRAME_BYTES as u64);
        }
        assert!(pager.contains(inner), "a scan flushed an inner page");
        assert!(pager.stats().evictions >= 30);
        // Hot leaves outlive scan pages.
        let hot = NodeAddr::new(2, 2);
        pager.insert(hot, Rc::new(page(hot)), Priority::Leaf);
        for unit in 50..53 {
            let addr = NodeAddr::new(1, unit);
            pager.insert(addr, Rc::new(page(addr)), Priority::Scan);
            pager.get(hot, Priority::Leaf);
        }
        assert!(pager.contains(hot));
    }

    #[test]
    fn pinned_pages_are_kept_over_budget() {
        let pager = Pager::new(FRAME_BYTES);
        let a = NodeAddr::new(1, 2);
        pager.insert(a, Rc::new(page(a)), Priority::Scan);
        let pin = pager.get(a, Priority::Scan).unwrap();
        let b = NodeAddr::new(1, 3);
        pager.insert(b, Rc::new(page(b)), Priority::Scan);
        assert!(pager.contains(a));
        assert_eq!(pager.stats().bytes, 2 * FRAME_BYTES as u64);
        drop(pin);
        let c = NodeAddr::new(1, 4);
        pager.insert(c, Rc::new(page(c)), Priority::Scan);
        assert!(pager.stats().bytes <= 2 * FRAME_BYTES as u64);
        pager.invalidate(c);
        assert!(!pager.contains(c));
    }

    #[test]
    fn faults_are_deduplicated_and_survive_a_dropped_leader() {
        let pager = Pager::new(16 * FRAME_BYTES);
        let addr = NodeAddr::new(1, 2);
        let FaultRole::Leader(leader) = pager.begin_fault(addr) else {
            panic!("first caller leads");
        };
        let FaultRole::Follower(follower) = pager.begin_fault(addr) else {
            panic!("second caller follows");
        };
        drop(leader);
        let mut wait = Box::pin(follower.wait());
        let cx = &mut std::task::Context::from_waker(Waker::noop());
        assert!(matches!(
            wait.as_mut().poll(cx),
            Poll::Ready(FaultResult::Retry)
        ));
        let FaultRole::Leader(leader) = pager.begin_fault(addr) else {
            panic!("retry leads");
        };
        let FaultRole::Follower(follower) = pager.begin_fault(addr) else {
            panic!("follows");
        };
        let mut wait = Box::pin(follower.wait());
        assert!(wait.as_mut().poll(cx).is_pending());
        leader.finish(Some(page(addr)), Priority::Leaf);
        assert!(matches!(
            wait.as_mut().poll(cx),
            Poll::Ready(FaultResult::Page(_))
        ));
        assert!(matches!(pager.begin_fault(addr), FaultRole::Hit(_)));
        assert_eq!(pager.stats().reads, 2);
    }
}
