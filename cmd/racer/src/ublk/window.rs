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
/// correct.
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

    /// Keep `page`, dropping the oldest pages past either bound.
    pub(crate) fn insert(&self, queue: u64, number: u64, page: P) {
        if self.queue_pages == 0 || self.worker_pages == 0 {
            return;
        }
        let mut entries = self.entries.borrow_mut();
        entries.retain(|e| !(e.queue == queue && e.number == number));
        if entries.iter().filter(|e| e.queue == queue).count() >= self.queue_pages
            && let Some(at) = entries.iter().position(|e| e.queue == queue)
        {
            entries.remove(at);
        }
        if entries.len() >= self.worker_pages {
            entries.pop_front();
        }
        entries.push_back(Entry {
            queue,
            number,
            page,
        });
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

    #[test]
    fn zero_bound_keeps_nothing() {
        let window = PageWindow::new(0, 4);
        let q = window.queue_id();
        window.insert(q, 0, 0);
        assert_eq!(window.len(), 0);
    }
}
