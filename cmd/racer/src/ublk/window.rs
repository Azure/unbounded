//! Plaintext pages a worker keeps across ublk requests.
//!
//! The page cache keeps only ciphertext and a weak handle to plaintext
//! (`memory::CachedPage`). Once no request holds a page's plaintext, the next
//! read of that page decrypts all 16 MiB again. Small sequential reads would
//! then decrypt one page per request. The window keeps the last pages each
//! queue used, so those reads share one decrypt.
//!
//! Each held page is a quota-charged plaintext owner, so the window is small
//! and bounded per worker across all its devices. A queue keeps at most
//! [`QUEUE_PAGES`] (the page it is reading and the one readahead fetched),
//! and a worker at most [`WORKER_PAGES`]. Past either bound the least
//! recently used page goes first. A queue drops its pages when it is
//! abandoned, finishes, or the control thread is done, so a held page never
//! delays shutdown, a quota stop, or the runtime fence.

use std::cell::{Cell, RefCell};
use std::collections::VecDeque;

/// Pages one queue keeps: the current page and the readahead page.
pub(crate) const QUEUE_PAGES: usize = 2;

/// Pages one worker keeps across all its queues. Two sequential readers on
/// one worker are served without extra decrypts, at 64 MiB of plaintext per
/// worker. More readers fall back to decrypting on a miss, which is still
/// correct. [`PageWindow::for_quota`] lowers this to fit the worker's
/// plaintext quota.
pub(crate) const WORKER_PAGES: usize = 4;

struct Entry<P> {
    queue: u64,
    number: u64,
    page: P,
}

/// Recently used pages of one worker, oldest first.
pub(crate) struct PageWindow<P> {
    entries: RefCell<VecDeque<Entry<P>>>,
    next_queue: Cell<u64>,
    queue_pages: usize,
    worker_pages: usize,
}

impl<P: Clone> Default for PageWindow<P> {
    fn default() -> Self {
        Self::new(QUEUE_PAGES, WORKER_PAGES)
    }
}

impl<P: Clone> PageWindow<P> {
    pub(crate) fn new(queue_pages: usize, worker_pages: usize) -> Self {
        Self {
            entries: RefCell::new(VecDeque::new()),
            next_queue: Cell::new(0),
            queue_pages,
            worker_pages,
        }
    }

    /// A window that never holds the plaintext a worker needs for reads.
    ///
    /// A worker's plaintext quota must fund `range_window_pages + 1` pages
    /// in flight (see `partition_limits_with_cause` in `app.rs`). The window
    /// keeps only what is left over, so at the minimum quota it keeps
    /// nothing and every read can still be admitted.
    pub(crate) fn for_quota(plaintext_bytes: usize, range_window_pages: usize) -> Self {
        let pages = plaintext_bytes / crate::model::PAGE_BYTES as usize;
        let spare = pages.saturating_sub(range_window_pages.saturating_add(1));
        let worker_pages = spare.min(WORKER_PAGES);
        Self::new(QUEUE_PAGES.min(worker_pages), worker_pages)
    }

    /// Pages the worker may keep across all queues.
    #[cfg(test)]
    pub(crate) fn worker_pages(&self) -> usize {
        self.worker_pages
    }

    /// A new id for one queue's pages.
    pub(crate) fn queue_id(&self) -> u64 {
        let id = self.next_queue.get();
        self.next_queue.set(id + 1);
        id
    }

    /// The kept page, marked as most recently used.
    pub(crate) fn get(&self, queue: u64, number: u64) -> Option<P> {
        let mut entries = self.entries.borrow_mut();
        let at = entries
            .iter()
            .position(|e| e.queue == queue && e.number == number)?;
        let entry = entries.remove(at)?;
        let page = entry.page.clone();
        entries.push_back(entry);
        Some(page)
    }

    /// Drop the page that keeping one more page of `queue` would evict.
    /// A miss calls this before fetching, so the evicted page's quota is
    /// free while the new page is admitted.
    pub(crate) fn make_room(&self, queue: u64) {
        let dropped = {
            let mut entries = self.entries.borrow_mut();
            Self::evict(&mut entries, queue, self.queue_pages, self.worker_pages)
        };
        drop(dropped);
    }

    fn evict(
        entries: &mut VecDeque<Entry<P>>,
        queue: u64,
        queue_pages: usize,
        worker_pages: usize,
    ) -> Vec<Entry<P>> {
        let mut dropped = Vec::new();
        while entries.iter().filter(|e| e.queue == queue).count() >= queue_pages.max(1)
            && let Some(at) = entries.iter().position(|e| e.queue == queue)
        {
            dropped.extend(entries.remove(at));
        }
        while entries.len() >= worker_pages.max(1)
            && let Some(entry) = entries.pop_front()
        {
            dropped.push(entry);
        }
        dropped
    }

    /// Keep `page`, dropping the oldest pages past either bound.
    pub(crate) fn insert(&self, queue: u64, number: u64, page: P) {
        if self.queue_pages == 0 || self.worker_pages == 0 {
            return;
        }
        let mut entries = self.entries.borrow_mut();
        entries.retain(|e| !(e.queue == queue && e.number == number));
        let dropped = Self::evict(&mut entries, queue, self.queue_pages, self.worker_pages);
        entries.push_back(Entry {
            queue,
            number,
            page,
        });
        drop(entries);
        drop(dropped);
    }

    /// Drop every page of every queue, for example when a fetch was refused
    /// for lack of quota.
    pub(crate) fn clear(&self) {
        let dropped = std::mem::take(&mut *self.entries.borrow_mut());
        drop(dropped);
    }

    /// Drop every page of one queue.
    pub(crate) fn release(&self, queue: u64) {
        // Pages are dropped after the borrow ends.
        let dropped: VecDeque<Entry<P>> = {
            let mut entries = self.entries.borrow_mut();
            let (dropped, keep) = entries.drain(..).partition(|e| e.queue == queue);
            *entries = keep;
            dropped
        };
        drop(dropped);
    }

    /// Pages kept for one queue.
    pub(crate) fn held(&self, queue: u64) -> usize {
        self.entries
            .borrow()
            .iter()
            .filter(|e| e.queue == queue)
            .count()
    }

    /// Whether no page is kept.
    pub(crate) fn is_empty(&self) -> bool {
        self.entries.borrow().is_empty()
    }

    /// Pages kept for all queues.
    #[cfg(test)]
    pub(crate) fn len(&self) -> usize {
        self.entries.borrow().len()
    }
}

#[cfg(test)]
mod tests {
    use std::rc::Rc;

    use super::*;

    #[test]
    fn queue_keeps_its_two_latest_pages() {
        let window = PageWindow::default();
        let q = window.queue_id();
        for n in 0..5 {
            window.insert(q, n, n);
        }
        assert_eq!(window.held(q), QUEUE_PAGES);
        assert_eq!(window.get(q, 2), None);
        assert_eq!(window.get(q, 3), Some(3));
        assert_eq!(window.get(q, 4), Some(4));
    }

    #[test]
    fn worker_bound_spans_queues_and_drops_the_oldest() {
        let window = PageWindow::default();
        let queues: Vec<_> = (0..3).map(|_| window.queue_id()).collect();
        for (i, q) in queues.iter().enumerate() {
            window.insert(*q, 0, i);
            window.insert(*q, 1, i);
        }
        assert_eq!(window.len(), WORKER_PAGES);
        assert_eq!(window.held(queues[0]), 0);
        assert_eq!(window.held(queues[2]), 2);
        // A hit makes a page the newest.
        assert_eq!(window.get(queues[1], 0), Some(1));
        window.insert(queues[0], 7, 0);
        assert_eq!(window.get(queues[1], 1), None);
        assert_eq!(window.get(queues[1], 0), Some(1));
    }

    #[test]
    fn release_drops_only_that_queue() {
        let window = PageWindow::default();
        let (a, b) = (window.queue_id(), window.queue_id());
        let page = Rc::new(());
        window.insert(a, 0, page.clone());
        window.insert(b, 0, page.clone());
        window.insert(a, 0, page.clone());
        assert_eq!(Rc::strong_count(&page), 3);
        window.release(a);
        assert_eq!(Rc::strong_count(&page), 2);
        assert_eq!(window.held(b), 1);
    }

    const PAGE: usize = crate::model::PAGE_BYTES as usize;

    #[test]
    fn quota_sizing_leaves_room_for_reads_in_flight() {
        // Minimum valid quota: range window plus one page. Nothing is kept.
        let window = PageWindow::<u64>::for_quota(2 * PAGE, 1);
        assert_eq!(window.worker_pages(), 0);
        let q = window.queue_id();
        window.insert(q, 0, 0);
        assert_eq!(window.len(), 0);
        // Less than a page past the minimum still keeps nothing.
        assert_eq!(
            PageWindow::<u64>::for_quota(4 * PAGE - 1, 2).worker_pages(),
            0
        );
        // One spare page: one page kept, one per queue.
        let window = PageWindow::<u64>::for_quota(3 * PAGE, 1);
        assert_eq!(window.worker_pages(), 1);
        let q = window.queue_id();
        window.insert(q, 0, 0);
        window.insert(q, 1, 1);
        assert_eq!(window.len(), 1);
        assert_eq!(window.get(q, 1), Some(1));
        // A large quota is capped at the default bounds.
        let window = PageWindow::<u64>::for_quota(64 * PAGE, 2);
        assert_eq!(window.worker_pages(), WORKER_PAGES);
        // Tiny or zero quotas do not underflow.
        assert_eq!(PageWindow::<u64>::for_quota(0, 64).worker_pages(), 0);
        assert_eq!(
            PageWindow::<u64>::for_quota(usize::MAX, usize::MAX).worker_pages(),
            0
        );
    }

    #[test]
    fn make_room_drops_what_insert_would_evict() {
        let window = PageWindow::default();
        let (a, b) = (window.queue_id(), window.queue_id());
        window.insert(a, 0, 0);
        window.insert(a, 1, 1);
        window.make_room(a);
        assert_eq!(window.held(a), QUEUE_PAGES - 1);
        assert_eq!(window.get(a, 0), None);
        window.insert(b, 0, 2);
        window.insert(b, 1, 3);
        window.insert(a, 2, 4);
        window.make_room(b);
        assert_eq!(window.len(), WORKER_PAGES - 1);
        window.clear();
        assert_eq!(window.len(), 0);
    }

    #[test]
    fn zero_bound_keeps_nothing() {
        let window = PageWindow::new(0, 4);
        let q = window.queue_id();
        window.insert(q, 0, 0);
        assert_eq!(window.len(), 0);
    }
}
