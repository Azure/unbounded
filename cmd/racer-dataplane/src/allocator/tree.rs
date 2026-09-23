// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Resident CoW tree mutation. Persistence encoding belongs to `disk`.
use super::*;
impl Node {
    pub(super) fn empty() -> Self {
        Self {
            body: Body::Leaf(Vec::new()),
            disk: None,
        }
    }
    pub(super) fn first(&self) -> Key {
        match &self.body {
            Body::Leaf(v) => v.first().map_or([0; 32], |v| v.0),
            Body::Branch(v) => v[0].first(),
        }
    }
    pub(super) fn len(&self) -> usize {
        match &self.body {
            Body::Leaf(v) => v.len(),
            Body::Branch(v) => v.len(),
        }
    }
    pub(super) fn height(&self) -> usize {
        match &self.body {
            Body::Leaf(_) => 0,
            Body::Branch(v) => 1 + v[0].height(),
        }
    }
    fn child(children: &[Rc<Node>], key: &Key) -> usize {
        children
            .partition_point(|c| c.first() <= *key)
            .saturating_sub(1)
    }
    pub(super) fn get(&self, key: &Key) -> Option<&Entry> {
        match &self.body {
            Body::Leaf(v) => v.binary_search_by_key(key, |v| v.0).ok().map(|i| &v[i].1),
            Body::Branch(v) => v[Self::child(v, key)].get(key),
        }
    }
    pub(super) fn insert(node: &mut Rc<Self>, key: Key, value: Entry) -> Option<Rc<Self>> {
        let node = Rc::make_mut(node);
        node.disk = None;
        match &mut node.body {
            Body::Leaf(v) => match v.binary_search_by_key(&key, |v| v.0) {
                Ok(i) => v[i].1 = value,
                Err(i) => v.insert(i, (key, value)),
            },
            Body::Branch(v) => {
                let i = Self::child(v, &key);
                if let Some(right) = Self::insert(&mut v[i], key, value) {
                    v.insert(i + 1, right);
                }
            }
        }
        if node.len() <= FANOUT {
            return None;
        }
        let body = match &mut node.body {
            Body::Leaf(v) => Body::Leaf(v.split_off(v.len() / 2)),
            Body::Branch(v) => Body::Branch(v.split_off(v.len() / 2)),
        };
        Some(Rc::new(Self { body, disk: None }))
    }
    pub(super) fn remove(node: &mut Rc<Self>, key: &Key) -> bool {
        if node.get(key).is_none() {
            return false;
        }
        let node = Rc::make_mut(node);
        node.disk = None;
        match &mut node.body {
            Body::Leaf(v) => {
                v.remove(v.binary_search_by_key(key, |v| v.0).unwrap());
            }
            Body::Branch(v) => {
                let i = Self::child(v, key);
                Self::remove(&mut v[i], key);
                if v[i].len() == 0 {
                    v.remove(i);
                } else if v.len() > 1 {
                    let left = i.min(v.len() - 2);
                    if v[i].len() < FANOUT.div_ceil(2) {
                        let right = v.remove(left + 1);
                        let target = Rc::make_mut(&mut v[left]);
                        target.disk = None;
                        match (&mut target.body, &right.body) {
                            (Body::Leaf(a), Body::Leaf(b)) => a.extend(b.iter().cloned()),
                            (Body::Branch(a), Body::Branch(b)) => a.extend(b.iter().cloned()),
                            _ => unreachable!(),
                        }
                        if target.len() > FANOUT {
                            let body = match &mut target.body {
                                Body::Leaf(a) => Body::Leaf(a.split_off(a.len() / 2)),
                                Body::Branch(a) => Body::Branch(a.split_off(a.len() / 2)),
                            };
                            v.insert(left + 1, Rc::new(Node { body, disk: None }));
                        }
                    }
                }
            }
        }
        true
    }
    pub(super) fn visit(&self, f: &mut impl FnMut(&Key, &Entry)) {
        match &self.body {
            Body::Leaf(v) => v.iter().for_each(|(k, v)| f(k, v)),
            Body::Branch(v) => v.iter().for_each(|v| v.visit(f)),
        }
    }
}
