//! Ordered shared-result cohorts without election or request-owned execution.
//!
//! The caller admits and owns real work. Dropping receivers cannot remove it.
//! Dropping a completion sender closes the shared receiver but deliberately keeps
//! its entry, matching an operation whose completion was never confirmed.

use futures::FutureExt;
use futures::channel::oneshot;
use futures::future::{LocalBoxFuture, Shared};
use std::cell::RefCell;
use std::collections::BTreeMap;
use std::rc::Rc;

pub type Receiver<V> = Shared<LocalBoxFuture<'static, V>>;

pub struct Table<K, V: Clone> {
    entries: RefCell<BTreeMap<K, Receiver<V>>>,
}
impl<K, V: Clone> Default for Table<K, V> {
    fn default() -> Self {
        Self {
            entries: RefCell::new(BTreeMap::new()),
        }
    }
}
impl<K: Ord + Clone, V: Clone + 'static> Table<K, V> {
    pub fn get(&self, key: &K) -> Option<Receiver<V>> {
        self.entries.borrow().get(key).cloned()
    }

    /// Called after miss-only admission, without yielding between get and start.
    /// The caller must not start a replacement while an entry is present.
    pub fn start(self: &Rc<Self>, key: K, closed: V) -> (Receiver<V>, Completion<K, V>) {
        let (send, receive) = oneshot::channel();
        let receive = async move { receive.await.unwrap_or(closed) }
            .boxed_local()
            .shared();
        self.entries
            .borrow_mut()
            .insert(key.clone(), receive.clone());
        (
            receive,
            Completion {
                table: self.clone(),
                key,
                send,
            },
        )
    }

    pub fn len(&self) -> usize {
        self.entries.borrow().len()
    }
    pub fn is_empty(&self) -> bool {
        self.entries.borrow().is_empty()
    }
}

pub struct Completion<K, V: Clone> {
    table: Rc<Table<K, V>>,
    key: K,
    send: oneshot::Sender<V>,
}
impl<K: Ord, V: Clone> Completion<K, V> {
    /// Real completion closes admission before notifying any old readers.
    pub fn finish(self, value: V) {
        self.table.entries.borrow_mut().remove(&self.key);
        let _ = self.send.send(value);
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use futures::executor::block_on;

    #[test]
    fn success_miss_failure_broadcast_and_replacement() {
        for value in [Ok(Some(7)), Ok(None), Err("io")] {
            let table = Rc::new(Table::default());
            let (first, completion) = table.start(1, Err("closed"));
            let second = table.get(&1).unwrap();
            assert_eq!(table.len(), 1);
            completion.finish(value);
            assert!(table.is_empty());
            let (next, completion) = table.start(1, Err("closed"));
            assert_eq!(block_on(first), value);
            assert_eq!(block_on(second), value);
            assert_eq!(table.len(), 1);
            completion.finish(Ok(Some(8)));
            assert_eq!(block_on(next), Ok(Some(8)));
        }
    }

    #[test]
    fn all_readers_drop_keeps_completion_owner_and_lost_sender_stays_closed() {
        let table = Rc::new(Table::default());
        let (receive, completion) = table.start(1, Err::<u32, _>("closed"));
        drop(receive);
        assert_eq!(table.len(), 1);
        let late = table.get(&1).unwrap();
        completion.finish(Ok(9));
        assert_eq!(block_on(late), Ok(9));
        assert!(table.is_empty());
        let (receive, completion) = table.start(1, Err("closed"));
        drop(completion);
        assert_eq!(block_on(receive), Err("closed"));
        assert_eq!(block_on(table.get(&1).unwrap()), Err("closed"));
        assert_eq!(table.len(), 1);
    }
}
