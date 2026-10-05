use super::*;
use std::cell::{Cell, RefCell};

struct Resource(Rc<Cell<usize>>);
impl Drop for Resource {
    fn drop(&mut self) {
        self.0.set(self.0.get() - 1);
    }
}

struct TestEntry {
    identity: Identity,
    waiters: usize,
    canceled: bool,
    refreshed: usize,
    operations: Operations<Resource>,
}
impl Entry for TestEntry {
    fn incarnation(&self) -> u64 {
        self.identity.incarnation
    }

    fn refresh(&mut self, _: &mut Vec<Waker>) {
        self.refreshed += 1;
        if self.canceled {
            self.waiters = 0;
        }
    }

    fn quiescent(&self) -> bool {
        self.waiters == 0 && self.operations.is_empty()
    }
}
type TestTable = Table<u32, TestEntry>;

fn insert(table: &mut TestTable, owner: &Rc<()>, key: u32) -> Identity {
    let identity = table.identity(owner.clone()).unwrap();
    table.insert(
        key,
        TestEntry {
            identity: identity.clone(),
            waiters: 1,
            canceled: false,
            refreshed: 0,
            operations: Operations::default(),
        },
    );
    identity
}

#[test]
fn canceled_entry_keeps_resources_until_two_phase_completion() {
    let owner = Rc::new(());
    let mut table = TestTable::default();
    insert(&mut table, &owner, 1);
    insert(&mut table, &owner, 2);
    let live = Rc::new(Cell::new(1));
    let entry = table.get_mut(&1).unwrap();
    entry.operations.insert(1, Resource(live.clone()));
    entry.canceled = true;
    let mut wakes = Vec::new();
    table.sweep(1, &mut wakes);
    assert_eq!(table.get(&1).unwrap().waiters, 0);
    assert_eq!(table.get(&2).unwrap().waiters, 1);
    assert_eq!(live.get(), 1, "cancellation is not completion");
    let resources = table.get_mut(&1).unwrap().operations.take(1).unwrap();
    assert!(!table.remove_quiescent(&1), "occupied during resource drop");
    drop(resources);
    assert_eq!(live.get(), 0);
    assert!(
        !table.remove_quiescent(&1),
        "completion must clear the tombstone"
    );
    table.get_mut(&1).unwrap().operations.complete(1).unwrap();
    assert!(table.remove_quiescent(&1));
    assert_eq!(table.len(), 1);
}

struct Waiter {
    table: Rc<RefCell<TestTable>>,
    key: u32,
}
impl Drop for Waiter {
    fn drop(&mut self) {
        let mut table = self.table.borrow_mut();
        table.get_mut(&self.key).unwrap().waiters -= 1;
        table.remove_quiescent(&self.key);
    }
}

#[test]
fn dropped_waiter_and_completion_token_do_not_release_owned_operations() {
    let owner = Rc::new(());
    let table = Rc::new(RefCell::new(TestTable::default()));
    let token = insert(&mut table.borrow_mut(), &owner, 1);
    let live = Rc::new(Cell::new(1));
    table
        .borrow_mut()
        .get_mut(&1)
        .unwrap()
        .operations
        .insert(1, Resource(live.clone()));
    drop(Waiter {
        table: table.clone(),
        key: 1,
    });
    drop(token);
    table.borrow_mut().sweep(100, &mut Vec::new());
    assert_eq!(table.borrow().len(), 1);
    assert_eq!(live.get(), 1);
    let resources = table
        .borrow_mut()
        .get_mut(&1)
        .unwrap()
        .operations
        .take(1)
        .unwrap();
    drop(resources);
    table
        .borrow_mut()
        .get_mut(&1)
        .unwrap()
        .operations
        .complete(1)
        .unwrap();
    table.borrow_mut().sweep(1, &mut Vec::new());
    assert!(table.borrow().is_empty());
    assert_eq!(live.get(), 0);
}

#[test]
fn reelection_and_recreation_fence_stale_completions() {
    let owner = Rc::new(());
    let mut table = TestTable::default();
    insert(&mut table, &owner, 1);
    let entry = table.get_mut(&1).unwrap();
    entry.identity.advance(2).unwrap();
    let old = entry.identity.clone();
    let live = Rc::new(Cell::new(1));
    entry.operations.insert(1, Resource(live.clone()));
    assert_eq!(
        entry.operations.complete(1),
        Err(Stale),
        "cannot skip resource release"
    );
    let resources = entry.operations.take(1).unwrap();
    assert!(matches!(entry.operations.take(1), Err(Stale)));
    drop(resources);
    entry.operations.complete(1).unwrap();
    assert_eq!(entry.operations.complete(1), Err(Stale));
    entry.identity.advance(2).unwrap();
    assert_eq!(old.validate(&entry.identity), Err(Stale));
    assert!(
        old.same_registration(&entry.identity),
        "waiter survives retry"
    );
    assert_eq!(entry.identity.advance(2), Err(Exhausted));
    assert_eq!(entry.identity.generation, 2);
    let mut wrong_owner = old.clone();
    wrong_owner.owner = Rc::new(());
    assert_eq!(wrong_owner.validate(&old), Err(Stale));
    entry.waiters = 0;
    assert!(table.remove_quiescent(&1));
    let new = insert(&mut table, &owner, 1);
    assert!(!old.same_registration(&new));
    assert_eq!(old.validate(&new), Err(Stale));
    assert_eq!(live.get(), 0);
}

#[test]
fn sweeps_are_budgeted_fair_and_remove_index_entries() {
    let owner = Rc::new(());
    let mut table = TestTable::default();
    for key in 1..=3 {
        insert(&mut table, &owner, key);
    }
    table.sweep(0, &mut Vec::new());
    assert!(table.values().all(|entry| entry.refreshed == 0));
    for key in 1..=3 {
        table.sweep(1, &mut Vec::new());
        assert_eq!(table.get(&key).unwrap().refreshed, 1);
    }
    table.get_mut(&2).unwrap().canceled = true;
    table.sweep(99, &mut Vec::new());
    assert!(!table.contains_key(&2));
    assert_eq!(table.sweep.len(), 2);
    assert!(table.values().all(|entry| entry.refreshed == 2));
    table.drain_waker = Some(Waker::noop().clone());
    table.stop(&mut Vec::new(), |entry, _| {
        entry.canceled = true;
    });
    let mut wakes = Vec::new();
    table.sweep(99, &mut wakes);
    assert!(table.is_empty());
    assert!(table.sweep.is_empty());
    assert_eq!(wakes.len(), 1);
    table.sweep(99, &mut wakes);
    assert_eq!(wakes.len(), 1, "drain wake is taken once");
}

#[test]
fn cursor_wraps_after_removal_and_counters_never_wrap() {
    let mut entries = BTreeMap::from([(1, ()), (2, ()), (3, ())]);
    let mut cursor = Cursor::default();
    assert_eq!(cursor.next(&entries).map(|(id, _)| id), Some(1));
    entries.remove(&1);
    assert_eq!(cursor.next(&entries).map(|(id, _)| id), Some(2));
    assert_eq!(cursor.next(&entries).map(|(id, _)| id), Some(3));
    assert_eq!(cursor.next(&entries).map(|(id, _)| id), Some(2));
    entries.clear();
    assert!(cursor.next(&entries).is_none());
    let mut counter = Counter(u64::MAX - 1);
    assert_eq!(counter.next_id(), Ok(u64::MAX));
    assert_eq!(counter.next_id(), Err(Exhausted));
    assert_eq!(counter.next_id(), Err(Exhausted));
    let mut table = TestTable::default();
    table.incarnation = counter;
    assert!(matches!(table.identity(Rc::new(())), Err(Exhausted)));
    assert!(table.is_empty());
}

#[test]
fn replacement_and_controlled_access_keep_sweep_membership_synchronized() {
    let owner = Rc::new(());
    let mut table = TestTable::default();
    assert!(table.get(&1).is_none());
    assert!(table.get_mut(&1).is_none());
    assert!(!table.remove_quiescent(&1));
    insert(&mut table, &owner, 1);
    let original = table.entries[&1].sweep_id;
    insert(&mut table, &owner, 1);
    let replacement = table.entries[&1].sweep_id;
    insert(&mut table, &owner, 2);
    assert_eq!(table.len(), 2);
    assert_eq!(table.values().count(), 2);
    assert!(!table.sweep.contains_key(&original));
    assert!(table.sweep.contains_key(&replacement));
    table.sweep(2, &mut Vec::new());
    assert!(table.values().all(|entry| entry.refreshed == 1));
    table.get_mut(&1).unwrap().waiters = 0;
    assert!(table.remove_quiescent(&1));
    assert!(!table.contains_key(&1));
    assert_eq!(table.sweep.len(), table.len());
    table.sweep(1, &mut Vec::new());
    assert_eq!(table.get(&2).unwrap().refreshed, 2);
}

#[test]
fn mutable_incarnation_cannot_remove_another_entries_sweep_slot() {
    let owner = Rc::new(());
    let mut table = TestTable::default();
    insert(&mut table, &owner, 1);
    let other = insert(&mut table, &owner, 2);
    table.get_mut(&1).unwrap().identity.incarnation = other.incarnation;
    table.get_mut(&1).unwrap().waiters = 0;
    assert!(table.remove_quiescent(&1));
    assert_eq!(table.sweep.len(), 1);
    table.sweep(1, &mut Vec::new());
    assert_eq!(table.get(&2).unwrap().refreshed, 1);
    table.get_mut(&2).unwrap().waiters = 0;
    table.sweep(1, &mut Vec::new());
    assert!(table.is_empty());
    assert!(table.sweep.is_empty());
}

#[test]
fn stop_identity_mutation_preserves_replacement_and_removal_membership() {
    let owner = Rc::new(());
    let mut table = TestTable::default();
    for key in 1..=3 {
        insert(&mut table, &owner, key);
    }
    table.stop(&mut Vec::new(), |entry, _| {
        entry.identity.incarnation = u64::MAX;
        entry.canceled = true;
    });
    insert(&mut table, &owner, 2);
    assert_eq!(table.sweep.len(), 3);
    table.sweep(3, &mut Vec::new());
    assert_eq!(table.len(), 1);
    assert_eq!(table.sweep.len(), 1);
    assert_eq!(table.get(&2).unwrap().refreshed, 1);
    table.get_mut(&2).unwrap().waiters = 0;
    assert!(table.remove_quiescent(&2));
    assert!(table.sweep.is_empty());
}

#[test]
fn duplicate_entry_incarnations_have_independent_bounded_sweeps() {
    let owner = Rc::new(());
    let mut table = TestTable::default();
    let identity = table.identity(owner).unwrap();
    for key in 1..=3 {
        table.insert(
            key,
            TestEntry {
                identity: identity.clone(),
                waiters: 1,
                canceled: false,
                refreshed: 0,
                operations: Operations::default(),
            },
        );
    }
    assert_eq!(table.sweep.len(), 3);
    table.sweep(0, &mut Vec::new());
    assert!(table.values().all(|entry| entry.refreshed == 0));
    for key in 1..=3 {
        table.sweep(1, &mut Vec::new());
        assert_eq!(table.get(&key).unwrap().refreshed, 1);
        assert_eq!(
            table.values().map(|entry| entry.refreshed).sum::<usize>(),
            key as usize
        );
    }
    table.get_mut(&2).unwrap().waiters = 0;
    assert!(table.remove_quiescent(&2));
    table.sweep(99, &mut Vec::new());
    assert_eq!(table.sweep.len(), 2);
    assert!(table.values().all(|entry| entry.refreshed == 2));
}

#[test]
fn sweep_ids_wrap_skip_occupied_slots_and_do_not_consume_incarnations() {
    let owner = Rc::new(());
    let mut table = TestTable::default();
    let first = insert(&mut table, &owner, 1);
    table.next_sweep_id = u64::MAX;
    let second = insert(&mut table, &owner, 2);
    assert_eq!(table.entries[&2].sweep_id, 0);
    let third = insert(&mut table, &owner, 3);
    assert_eq!(table.entries[&3].sweep_id, 2, "skip occupied ID 1");
    assert_eq!(
        (first.incarnation, second.incarnation, third.incarnation),
        (1, 2, 3)
    );
    table.sweep(3, &mut Vec::new());
    assert!(table.values().all(|entry| entry.refreshed == 1));
    table.incarnation = Counter(u64::MAX);
    assert!(table.identity(owner).is_err());
    table.insert(
        4,
        TestEntry {
            identity: first,
            waiters: 1,
            canceled: false,
            refreshed: 0,
            operations: Operations::default(),
        },
    );
    assert_eq!(
        table.len(),
        4,
        "insert does not require an incarnation allocation"
    );
    table.stop(&mut Vec::new(), |entry, _| entry.canceled = true);
    table.sweep(4, &mut Vec::new());
    assert!(table.is_empty());
    assert!(table.sweep.is_empty());
}
