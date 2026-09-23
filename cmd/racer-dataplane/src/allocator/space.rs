// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Extent ownership, delayed pin retirement, and free-space accounting.

use super::*;

impl Space {
    pub(super) fn new(shard: &SlabShard) -> Rc<Self> {
        Rc::new(Self {
            geometry: shard.geometry,
            maps: [Class::Index, Class::Payload]
                .map(|c| RefCell::new(Bitmap::new(shard.geometry.range(c).1))),
            _file: shard.file.clone(),
            retired: RefCell::new(Vec::new()),
        })
    }

    fn reclaim_unpinned(&self) {
        self.retired.borrow_mut().retain(|(class, index, pin)| {
            if pin.strong_count() == 0 {
                self.maps[class.index()].borrow_mut().release(*index);
                false
            } else {
                true
            }
        });
    }

    pub(super) fn allocate(self: &Rc<Self>, class: Class) -> io::Result<Rc<Allocation>> {
        self.reclaim_unpinned();
        let index = self.maps[class.index()]
            .borrow_mut()
            .take()
            .ok_or_else(busy)?;
        Ok(Rc::new(Allocation {
            space: self.clone(),
            class,
            index,
            pin: Arc::new(()),
        }))
    }

    fn retire_allocation(&self, class: Class, index: usize, pin: &Arc<()>) {
        if Arc::strong_count(pin) != 1 {
            self.retired
                .borrow_mut()
                .push((class, index, Arc::downgrade(pin)));
        } else {
            self.maps[class.index()].borrow_mut().release(index);
        }
    }
}

impl Allocation {
    pub(super) fn page(&self) -> usize {
        let (start, _, stride) = self.space.geometry.range(self.class);
        start + self.index * stride
    }
    pub(super) fn offset(&self) -> u64 {
        self.space.geometry.offset(self.page())
    }
}

impl Drop for Allocation {
    fn drop(&mut self) {
        self.space
            .retire_allocation(self.class, self.index, &self.pin);
    }
}
