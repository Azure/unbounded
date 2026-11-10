//! Shared-result cohorts whose callers own execution rather than electing leaders.
//! Dropping receivers cannot remove real work. Dropping a completion sender
//! closes receivers but keeps its entry until completion is confirmed.

use futures::FutureExt;
use futures::channel::oneshot;
use futures::future::{LocalBoxFuture, Shared};
use std::cell::RefCell;
use std::collections::BTreeMap;
use std::rc::Rc;

/// Cloneable, worker-local result receiver independent of execution ownership.
pub type Receiver<V> = Shared<LocalBoxFuture<'static, V>>;

/// Ordered index retaining work until its completion owner explicitly finishes.
pub struct Table<K, V: Clone> {
    entries: RefCell<BTreeMap<K, Receiver<V>>>,
}

impl<K, V: Clone> Default for Table<K, V> {
    /// Create an empty operation index.
    fn default() -> Self {
        Self {
            entries: RefCell::new(BTreeMap::new()),
        }
    }
}

#[cfg(test)]
impl<K, V: Clone> Table<K, V> {
    /// Count operations, including those whose completion sender was lost.
    pub fn len(&self) -> usize {
        self.entries.borrow().len()
    }

    /// Whether no operation remains indexed.
    pub fn is_empty(&self) -> bool {
        self.entries.borrow().is_empty()
    }
}

impl<K: Ord, V: Clone> Table<K, V> {
    /// Join an existing result without taking ownership of its execution.
    pub fn get(&self, key: &K) -> Option<Receiver<V>> {
        self.entries.borrow().get(key).cloned()
    }
}

impl<K: Ord + Clone, V: Clone + 'static> Table<K, V> {
    /// Called after miss-only admission, without yielding between get and start.
    /// Recheck after key cloning: an occupied key joins without completion
    /// authority. Start work only when a completion owner is returned.
    pub fn start(self: &Rc<Self>, key: K, closed: V) -> (Receiver<V>, Option<Completion<K, V>>) {
        let index_key = key.clone();
        let (send, receive) = oneshot::channel();
        let receive = async move { receive.await.unwrap_or(closed) }
            .boxed_local()
            .shared();
        let existing = {
            let mut entries = self.entries.borrow_mut();
            // Keep the unused key and receiver outside this borrow on a join.
            if let Some(existing) = entries.get(&index_key) {
                Some(existing.clone())
            } else {
                entries.insert(index_key, receive.clone());
                None
            }
        };
        if let Some(existing) = existing {
            return (existing, None);
        }
        (
            receive,
            Some(Completion {
                table: self.clone(),
                key,
                send,
            }),
        )
    }
}

/// Sole completion authority. Dropping it reports closure, not real completion,
/// so its table entry deliberately remains occupied.
pub struct Completion<K, V: Clone> {
    table: Rc<Table<K, V>>,

    key: K,

    send: oneshot::Sender<V>,
}

impl<K: Ord, V: Clone> Completion<K, V> {
    /// Real completion closes admission before notifying any old readers.
    pub fn finish(self, value: V) {
        let removed = self.table.entries.borrow_mut().remove(&self.key);
        // Final receiver destruction can reenter the table.
        drop(removed);
        let _ = self.send.send(value);
    }
}

/// Shared-result ownership and replacement contracts.
#[cfg(test)]
mod tests {
    use super::*;
    use futures::executor::block_on;

    #[test]
    fn occupied_start_drops_unused_key_and_closed_value_outside_borrow() {
        use std::cell::Cell;

        thread_local! {
            static ON_KEY_DROP: RefCell<Option<Box<dyn FnOnce()>>> = RefCell::new(None);
        }

        #[derive(Clone, Eq, PartialEq, Ord, PartialOrd)]
        struct Key(u32);

        impl Drop for Key {
            fn drop(&mut self) {
                let callback = ON_KEY_DROP.with(|slot| slot.borrow_mut().take());
                if let Some(callback) = callback {
                    callback();
                }
            }
        }

        struct Closed(Option<Box<dyn FnOnce()>>);

        impl Drop for Closed {
            fn drop(&mut self) {
                if let Some(callback) = self.0.take() {
                    callback();
                }
            }
        }

        let table = Rc::new(Table::default());
        let (first, completion) = table.start(Key(1), Rc::new(Closed(None)));
        drop(first);
        let key_dropped = Rc::new(Cell::new(false));
        let closed_dropped = Rc::new(Cell::new(false));
        ON_KEY_DROP.with(|slot| {
            let table = table.clone();
            let observed = key_dropped.clone();
            *slot.borrow_mut() = Some(Box::new(move || {
                assert_eq!(table.entries.borrow_mut().len(), 1);
                observed.set(true);
            }));
        });
        let closed = {
            let table = table.clone();
            let observed = closed_dropped.clone();
            Rc::new(Closed(Some(Box::new(move || {
                assert_eq!(table.entries.borrow_mut().len(), 1);
                observed.set(true);
            }))))
        };
        let (joined, rejected) = table.start(Key(1), closed);
        assert!(rejected.is_none());
        assert!(key_dropped.get());
        assert!(closed_dropped.get());
        let result = Rc::new(Closed(None));
        completion.unwrap().finish(result.clone());
        assert!(Rc::ptr_eq(&block_on(joined), &result));
        assert!(table.is_empty());
    }

    /// Final receiver destruction can reenter through its retained closed value.
    #[test]
    fn completion_drops_last_receiver_outside_table_borrow() {
        use std::cell::Cell;
        use std::sync::Arc;
        use std::task::{Context, Wake, Waker};

        thread_local! {
            static ON_DROP: RefCell<Option<Box<dyn FnOnce()>>> = RefCell::new(None);
        }

        struct ReentrantDrop;

        impl Wake for ReentrantDrop {
            fn wake(self: Arc<Self>) {
                panic!("closed result waker must only be dropped");
            }
        }

        impl Drop for ReentrantDrop {
            fn drop(&mut self) {
                let callback = ON_DROP.with(|slot| slot.borrow_mut().take());
                if let Some(callback) = callback {
                    callback();
                }
            }
        }

        let table = Rc::new(Table::default());
        let dropped = Rc::new(Cell::new(false));
        let closed = Some(Waker::from(Arc::new(ReentrantDrop)));
        let (mut receive, completion) = table.start(1, closed);
        assert!(
            receive
                .poll_unpin(&mut Context::from_waker(Waker::noop()))
                .is_pending()
        );
        drop(receive);
        assert_eq!(table.len(), 1);

        let nested = table.clone();
        let observed = dropped.clone();
        ON_DROP.with(|slot| {
            *slot.borrow_mut() = Some(Box::new(move || {
                assert!(nested.is_empty());
                assert!(nested.get(&1).is_none());
                let (replacement, completion) = nested.start(1, None);
                assert_eq!(nested.len(), 1);
                completion.unwrap().finish(None);
                assert!(block_on(replacement).is_none());
                observed.set(true);
            }));
        });

        assert!(!dropped.get());
        completion.unwrap().finish(None);
        assert!(dropped.get());
        assert!(table.is_empty());
        assert!(ON_DROP.with(|slot| slot.borrow().is_none()));
    }

    /// Results remain readable after completion admits a replacement.
    #[test]
    fn success_miss_failure_broadcast_and_replacement() {
        for value in [Ok(Some(7)), Ok(None), Err("io")] {
            let table = Rc::new(Table::default());
            let (first, completion) = table.start(1, Err("closed"));
            let second = table.get(&1).unwrap();
            assert_eq!(table.len(), 1);
            completion.unwrap().finish(value);
            assert!(table.is_empty());
            let (next, completion) = table.start(1, Err("closed"));
            assert_eq!(block_on(first), value);
            assert_eq!(block_on(second), value);
            assert_eq!(table.len(), 1);
            completion.unwrap().finish(Ok(Some(8)));
            assert_eq!(block_on(next), Ok(Some(8)));
        }
    }

    /// Neither receiver cancellation nor sender loss pretends work completed.
    #[test]
    fn all_readers_drop_keeps_completion_owner_and_lost_sender_stays_closed() {
        let table = Rc::new(Table::default());
        let (receive, completion) = table.start(1, Err::<u32, _>("closed"));
        drop(receive);
        assert_eq!(table.len(), 1);
        let late = table.get(&1).unwrap();
        completion.unwrap().finish(Ok(9));
        assert_eq!(block_on(late), Ok(9));
        assert!(table.is_empty());
        let (receive, completion) = table.start(1, Err("closed"));
        drop(completion);
        assert_eq!(block_on(receive), Err("closed"));
        assert_eq!(block_on(table.get(&1).unwrap()), Err("closed"));
        assert_eq!(table.len(), 1);
    }

    use std::cell::{Cell, RefCell};
    use std::future::Future;
    use std::sync::Arc;
    use std::task::{Context, Wake, Waker};

    thread_local! {
        static ON_WAKE: RefCell<Option<Box<dyn FnOnce()>>> = RefCell::new(None);
    }

    /// Safe thread-local callback dispatch; no non-Send data enters the Waker itself.
    struct Reenter;

    impl Wake for Reenter {
        /// Invoke the current worker's callback after releasing its registry borrow.
        fn wake(self: Arc<Self>) {
            let callback = ON_WAKE.with(|slot| slot.borrow_mut().take());
            if let Some(callback) = callback {
                callback();
            }
        }
    }

    /// Install a callback and return a wake target that dispatches it synchronously.
    fn on_wake(callback: impl FnOnce() + 'static) -> Waker {
        ON_WAKE.with(|slot| assert!(slot.borrow_mut().replace(Box::new(callback)).is_none()));
        Waker::from(Arc::new(Reenter))
    }

    /// Shared result notification can synchronously start replacement work.
    #[test]
    fn shared_completion_wakes_after_removal_and_keeps_new_owner() {
        let table = Rc::new(Table::default());
        let (mut receive, complete) = table.start(1, 99);
        let replacement = Rc::new(RefCell::new(None));
        let waker = on_wake({
            let table = table.clone();
            let replacement = replacement.clone();
            move || {
                assert!(table.is_empty());
                replacement.replace(Some(table.start(1, 99)));
            }
        });
        assert!(
            std::pin::Pin::new(&mut receive)
                .poll(&mut Context::from_waker(&waker))
                .is_pending()
        );
        complete.unwrap().finish(7);
        assert_eq!(block_on(receive), 7);
        let (receive, complete) = replacement.borrow_mut().take().unwrap();
        assert_eq!(table.len(), 1);
        drop(complete);
        assert_eq!(block_on(receive), 99);
        assert_eq!(
            table.len(),
            1,
            "sender loss cannot pretend execution completed"
        );
    }

    /// Losing the sender wakes parked readers without removing the indexed work.
    #[test]
    fn shared_sender_drop_wakes_with_entry_still_indexed() {
        let table = Rc::new(Table::default());
        let (mut receive, complete) = table.start(1, 99);
        let notified = Rc::new(Cell::new(false));
        let waker = on_wake({
            let table = table.clone();
            let notified = notified.clone();
            move || {
                assert_eq!(table.len(), 1);
                assert!(table.get(&1).is_some());
                notified.set(true);
            }
        });
        assert!(
            std::pin::Pin::new(&mut receive)
                .poll(&mut Context::from_waker(&waker))
                .is_pending()
        );
        drop(complete);
        assert!(notified.get());
        assert_eq!(block_on(receive), 99);
        assert_eq!(block_on(table.get(&1).unwrap()), 99);
        assert_eq!(table.len(), 1);
    }
}

/// Key-clone reentrancy contracts.
#[cfg(test)]
mod clone_reentry_tests {
    use super::*;
    use std::cell::RefCell;
    use std::rc::Rc;

    thread_local! {
        static ON_KEY_CLONE: RefCell<Option<Box<dyn FnOnce()>>> = RefCell::new(None);
    }

    /// A key whose next clone can call back into its table.
    #[derive(Debug, Eq, PartialEq, Ord, PartialOrd, Hash)]
    struct CloneKey(u32);

    impl Clone for CloneKey {
        /// Run the one-shot callback without retaining the callback slot borrow.
        fn clone(&self) -> Self {
            let callback = ON_KEY_CLONE.with(|slot| slot.borrow_mut().take());
            if let Some(callback) = callback {
                callback();
            }
            Self(self.0)
        }
    }

    /// Shared key cloning can inspect the table and start unrelated work.
    #[test]
    fn key_clone_reentry_shared_start_preserves_both_completions() {
        use futures::executor::block_on;

        let table = Rc::new(Table::default());
        let admitted = Rc::new(RefCell::new(None));
        let nested = table.clone();
        let retained = admitted.clone();
        ON_KEY_CLONE.with(|slot| {
            *slot.borrow_mut() = Some(Box::new(move || {
                assert!(nested.is_empty());
                assert!(nested.get(&CloneKey(1)).is_none());
                *retained.borrow_mut() = Some(nested.start(CloneKey(2), 0));
            }));
        });
        let (outer, completion) = table.start(CloneKey(1), 0);
        let (inner, nested_completion) = admitted.borrow_mut().take().unwrap();
        assert_eq!(table.len(), 2);
        completion.unwrap().finish(7);
        assert_eq!(block_on(outer), 7);
        assert_eq!(table.len(), 1);
        assert!(table.get(&CloneKey(1)).is_none());
        let follower = table.get(&CloneKey(2)).unwrap();
        nested_completion.unwrap().finish(8);
        assert_eq!(block_on(inner), 8);
        assert_eq!(block_on(follower), 8);
        assert!(table.is_empty());
    }

    #[test]
    fn shared_same_key_clone_reentry_joins_nested_completion() {
        use futures::FutureExt;

        for result in [Ok(7), Err("failed")] {
            let table = Rc::new(Table::default());
            let admitted = Rc::new(RefCell::new(None));
            let nested = table.clone();
            let retained = admitted.clone();
            ON_KEY_CLONE.with(|slot| {
                *slot.borrow_mut() = Some(Box::new(move || {
                    assert!(nested.get(&CloneKey(1)).is_none());
                    *retained.borrow_mut() = Some(nested.start(CloneKey(1), Err("nested closed")));
                }));
            });
            assert!(table.get(&CloneKey(1)).is_none());
            let (outer, completion) = table.start(CloneKey(1), Err("outer closed"));
            let (inner, nested_completion) = admitted.borrow_mut().take().unwrap();
            let follower = table.get(&CloneKey(1)).unwrap();
            assert_eq!(table.len(), 1);
            assert!(completion.is_none());
            nested_completion.unwrap().finish(result);
            assert_eq!(inner.now_or_never(), Some(result));
            assert_eq!(outer.now_or_never(), Some(result));
            assert_eq!(follower.now_or_never(), Some(result));
            assert!(table.is_empty());
            let (next, next_completion) = table.start(CloneKey(1), Err("next closed"));
            drop(completion);
            assert_eq!(table.len(), 1);
            next_completion.unwrap().finish(Ok(8));
            assert_eq!(next.now_or_never(), Some(Ok(8)));
            assert!(table.is_empty());
        }
    }

    #[test]
    fn shared_same_key_clone_reentry_completed_before_start_keeps_new_owner() {
        use futures::FutureExt;

        let table = Rc::new(Table::default());
        let admitted = Rc::new(RefCell::new(None));
        let nested = table.clone();
        let retained = admitted.clone();
        ON_KEY_CLONE.with(|slot| {
            *slot.borrow_mut() = Some(Box::new(move || {
                let (receive, completion) = nested.start(CloneKey(1), 99);
                completion.unwrap().finish(7);
                *retained.borrow_mut() = Some(receive);
                assert!(nested.is_empty());
            }));
        });
        let (outer, completion) = table.start(CloneKey(1), 99);
        assert_eq!(table.len(), 1);
        let inner = admitted.borrow_mut().take().unwrap();
        assert_eq!(inner.now_or_never(), Some(7));
        let follower = table.get(&CloneKey(1)).unwrap();
        assert!(outer.clone().now_or_never().is_none());
        completion.unwrap().finish(8);
        assert_eq!(outer.now_or_never(), Some(8));
        assert_eq!(follower.now_or_never(), Some(8));
        assert!(table.is_empty());
    }

    #[test]
    fn shared_same_key_clone_reentry_lost_sender_stays_occupied() {
        use futures::FutureExt;

        let table = Rc::new(Table::default());
        let nested = table.clone();
        ON_KEY_CLONE.with(|slot| {
            *slot.borrow_mut() = Some(Box::new(move || {
                let (receive, completion) = nested.start(CloneKey(1), 7);
                drop((receive, completion));
                assert_eq!(nested.len(), 1);
            }));
        });
        let (outer, completion) = table.start(CloneKey(1), 99);
        assert!(completion.is_none());
        assert_eq!(outer.now_or_never(), Some(7));
        assert_eq!(table.len(), 1);
        let (late, completion) = table.start(CloneKey(1), 88);
        assert!(completion.is_none());
        assert_eq!(late.now_or_never(), Some(7));
        assert_eq!(table.len(), 1);
    }
}
