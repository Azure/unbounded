// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Byte format and validated recovery of both retained roots.
use super::*;
pub(super) fn put(page: &mut uring::Page, at: usize, value: u64) {
    page.0[at..at + 8].copy_from_slice(&value.to_le_bytes());
}
pub(super) fn get(page: &uring::Page, at: usize) -> u64 {
    u64::from_le_bytes(page.0[at..at + 8].try_into().unwrap())
}

// CRC64/ECMA-182 with runtime CPU dispatch and a software fallback.
// Values may supply an already computed checksum.
pub fn crc64(bytes: &[u8]) -> u64 {
    crc_fast::checksum(crc_fast::CrcAlgorithm::Crc64Ecma182, bytes)
}
pub(super) fn seal(page: &mut uring::Page) {
    put(page, 8, 0);
    put(page, 8, crc64(&page.0));
}
pub(super) fn valid(page: &mut uring::Page, tag: u64) -> bool {
    let checksum = get(page, 8);
    put(page, 8, 0);
    let ok = get(page, 0) == tag && crc64(&page.0) == checksum;
    put(page, 8, checksum);
    ok
}
pub(super) fn magic(
    g: Geometry,
    generation: u64,
    root: u64,
    bitmaps: &[Rc<Allocation>],
) -> Box<uring::Page> {
    let mut page = Box::new(uring::Page([0; PAGE_SIZE]));
    for (i, value) in [
        MAGIC,
        0,
        generation,
        g.base,
        g.len,
        g.shard,
        g.count,
        root,
        bitmaps.len() as u64,
    ]
    .into_iter()
    .enumerate()
    {
        put(&mut page, i * 8, value);
    }
    for (i, bitmap) in bitmaps.iter().enumerate() {
        put(&mut page, ROOT_HEADER_BYTES + i * 8, bitmap.page() as u64);
    }
    seal(&mut page);
    page
}
pub(super) fn encode(node: &Node) -> Box<uring::Page> {
    let mut page = Box::new(uring::Page([0; PAGE_SIZE]));
    put(&mut page, 0, NODE);
    put(
        &mut page,
        16,
        u64::from(matches!(node.body, Body::Branch(_))),
    );
    put(&mut page, 24, node.len() as u64);
    match &node.body {
        Body::Leaf(v) => {
            for (i, (key, value)) in v.iter().enumerate() {
                let at = 32 + i * LEAF_ENTRY;
                page.0[at..at + 32].copy_from_slice(key);
                match value {
                    Entry::Metadata(metadata) => {
                        page.0[at + 40..at + LEAF_ENTRY].copy_from_slice(&metadata.to_bytes());
                    }
                    Entry::Payload(value) => {
                        put(&mut page, at + 32, 1);
                        put(&mut page, at + 40, value.allocation.page() as u64);
                        put(&mut page, at + 48, value.info.len as u64);
                        put(&mut page, at + 56, value.info.crc64);
                    }
                }
            }
        }
        Body::Branch(v) => {
            for (i, child) in v.iter().enumerate() {
                let at = 32 + i * 40;
                page.0[at..at + 32].copy_from_slice(&child.first());
                put(
                    &mut page,
                    at + 32,
                    child.disk.as_ref().unwrap().page() as u64,
                );
            }
        }
    }
    seal(&mut page);
    page
}

pub(super) struct Checkpoint {
    pub(super) generation: u64,
    pub(super) root: Rc<Node>,
    pub(super) bitmaps: Vec<(Rc<Allocation>, Box<uring::Page>)>,
}

// Recovery interns allocations shared by the two checkpoints. A tag prevents
// the same physical extent from being accepted with conflicting identities.
pub(super) type Intern = HashMap<usize, (u64, Weak<Allocation>)>;
fn claim(
    space: &Rc<Space>,
    intern: &mut Intern,
    page: usize,
    class: Class,
    tag: u64,
) -> io::Result<Rc<Allocation>> {
    let (start, count, stride) = space.geometry.range(class);
    if page < start || !(page - start).is_multiple_of(stride) || (page - start) / stride >= count {
        return Err(invalid("allocation outside its size class"));
    }
    if let Some((old, allocation)) = intern.get(&page)
        && let Some(allocation) = allocation.upgrade()
    {
        if *old != tag || allocation.class != class {
            return Err(invalid("conflicting checkpoint allocations"));
        }
        return Ok(allocation);
    }
    let index = (page - start) / stride;
    space.maps[class.index()].borrow_mut().set(index, false);
    let allocation = Rc::new(Allocation {
        space: space.clone(),
        class,
        index,
        pin: Arc::new(()),
    });
    intern.insert(page, (tag, Rc::downgrade(&allocation)));
    Ok(allocation)
}
pub(super) fn read_page(file: &SlabFile, g: Geometry, page: usize) -> io::Result<Box<uring::Page>> {
    if page >= g.pages() {
        return Err(invalid("page outside shard"));
    }
    let mut bytes = Box::new(uring::Page([0; PAGE_SIZE]));
    file.read_exact_at(&mut bytes.0, g.offset(page))?;
    Ok(bytes)
}
fn mark(bits: &mut [u8], page: usize, count: usize) -> io::Result<()> {
    for i in page..page + count {
        let mask = 1 << (i % 8);
        if bits[i / 8] & mask != 0 {
            return Err(invalid("overlapping or cyclic checkpoint"));
        }
        bits[i / 8] |= mask;
    }
    Ok(())
}
fn load_node(
    file: &SlabFile,
    space: &Rc<Space>,
    intern: &mut Intern,
    used: &mut [u8],
    page: usize,
    depth: usize,
    metadata_remaining: &mut usize,
) -> io::Result<Rc<Node>> {
    if depth > 16 {
        return Err(invalid("tree too deep"));
    }
    let mut bytes = read_page(file, space.geometry, page)?;
    if !valid(&mut bytes, NODE) {
        return Err(invalid("invalid tree checksum"));
    }
    let allocation = claim(space, intern, page, Class::Index, get(&bytes, 8))?;
    mark(used, page, 1)?;
    let count = get(&bytes, 24) as usize;
    if count == 0 || count > FANOUT || (depth != 0 && count < FANOUT.div_ceil(2)) {
        return Err(invalid("invalid tree occupancy"));
    }
    let mut previous = None;
    let body = match get(&bytes, 16) {
        0 => {
            let mut values = Vec::with_capacity(count);
            for i in 0..count {
                let at = 32 + i * LEAF_ENTRY;
                let key: Key = bytes.0[at..at + 32].try_into().unwrap();
                if previous.is_some_and(|p| p >= key) {
                    return Err(invalid("unsorted leaf"));
                }
                previous = Some(key);
                let value = match get(&bytes, at + 32) {
                    0 => {
                        *metadata_remaining = metadata_remaining
                            .checked_sub(1)
                            .ok_or_else(|| invalid("checkpoint exceeds metadata entry bound"))?;
                        let metadata = Metadata::from_bytes(&bytes.0[at + 40..at + LEAF_ENTRY])?;
                        if metadata.expires == 0 {
                            return Err(invalid("request-scoped metadata in checkpoint"));
                        }
                        Entry::Metadata(metadata)
                    }
                    1 => {
                        if bytes.0[at + 64..at + LEAF_ENTRY].iter().any(|b| *b != 0) {
                            return Err(invalid("nonzero payload descriptor padding"));
                        }
                        let info = ValueInfo {
                            kind: Kind::Payload,
                            len: get(&bytes, at + 48) as usize,
                            crc64: get(&bytes, at + 56),
                            expires: 0,
                        };
                        validate_info(info)?;
                        let allocation = claim(
                            space,
                            intern,
                            get(&bytes, at + 40) as usize,
                            Class::Payload,
                            crc64(&bytes.0[at..at + LEAF_ENTRY]),
                        )?;
                        mark(used, allocation.page(), PAYLOAD_PAGES)?;
                        Entry::Payload(Rc::new(PayloadExtent {
                            allocation,
                            info,
                            buffer: RefCell::new(None),
                            written: Cell::new(true),
                        }))
                    }
                    _ => return Err(invalid("invalid value kind")),
                };
                values.push((key, value));
            }
            Body::Leaf(values)
        }
        1 => {
            if count < 2 {
                return Err(invalid("unary tree root"));
            }
            let mut children = Vec::with_capacity(count);
            for i in 0..count {
                let at = 32 + i * 40;
                let key: Key = bytes.0[at..at + 32].try_into().unwrap();
                if previous.is_some_and(|p| p >= key) {
                    return Err(invalid("unsorted branch"));
                }
                let child = load_node(
                    file,
                    space,
                    intern,
                    used,
                    get(&bytes, at + 32) as usize,
                    depth + 1,
                    metadata_remaining,
                )?;
                if child.first() != key {
                    return Err(invalid("invalid branch separator"));
                }
                if children
                    .first()
                    .is_some_and(|c: &Rc<Node>| c.height() != child.height())
                {
                    return Err(invalid("unbalanced tree"));
                }
                previous = Some(key);
                children.push(child);
            }
            Body::Branch(children)
        }
        _ => return Err(invalid("invalid tree kind")),
    };
    Ok(Rc::new(Node {
        body,
        disk: Some(allocation),
    }))
}
pub(super) fn validate_info(info: ValueInfo) -> io::Result<()> {
    if info.len == 0 || info.len > BUFFER_SIZE || info.kind != Kind::Payload || info.expires != 0 {
        return Err(invalid("invalid cache value length or expiration"));
    }
    Ok(())
}

pub(super) fn recover(
    file: &SlabFile,
    space: &Rc<Space>,
    intern: &mut Intern,
    slot: usize,
) -> io::Result<Checkpoint> {
    let g = space.geometry;
    let mut page = read_page(file, g, slot)?;
    if !valid(&mut page, MAGIC)
        || get(&page, 16) == 0
        || [
            get(&page, 24),
            get(&page, 32),
            get(&page, 40),
            get(&page, 48),
        ] != [g.base, g.len, g.shard, g.count]
    {
        return Err(invalid("invalid shard magic or geometry"));
    }
    let mut used = vec![0u8; g.pages().div_ceil(8)];
    let mut metadata_remaining = g.metadata_limit();
    let root = if get(&page, 56) == 0 {
        Rc::new(Node::empty())
    } else {
        load_node(
            file,
            space,
            intern,
            &mut used,
            get(&page, 56) as usize,
            0,
            &mut metadata_remaining,
        )?
    };
    // Check cross-child ordering as well as local separator ordering.
    let mut previous = None;
    let mut sorted = true;
    root.visit(&mut |key, _| {
        sorted &= previous.is_none_or(|p| p < *key);
        previous = Some(*key);
    });
    if !sorted {
        return Err(invalid("overlapping tree key ranges"));
    }
    let count = get(&page, 64) as usize;
    if count > ROOT_BITMAP_SLOTS
        || (count != used.len().div_ceil(BIT_BYTES) && !(count == 0 && root.len() == 0))
    {
        return Err(invalid("invalid bitmap length"));
    }
    let mut bitmaps = Vec::with_capacity(count);
    for i in 0..count {
        let position = get(&page, ROOT_HEADER_BYTES + i * 8) as usize;
        let mut bitmap = read_page(file, g, position)?;
        if !valid(&mut bitmap, BITS) || get(&bitmap, 16) != i as u64 {
            return Err(invalid("invalid bitmap page"));
        }
        let start = i * BIT_BYTES;
        let len = BIT_BYTES.min(used.len() - start);
        if bitmap.0[32..32 + len] != used[start..start + len] {
            return Err(invalid("bitmap disagrees with tree"));
        }
        let allocation = claim(space, intern, position, Class::Index, get(&bitmap, 8))?;
        // Separate bitmap-page set: bitmap bits describe tree and values only.
        if used[position / 8] & (1 << (position % 8)) != 0
            || bitmaps
                .iter()
                .any(|(a, _): &(Rc<Allocation>, Box<uring::Page>)| a.page() == position)
        {
            return Err(invalid("bitmap overlaps checkpoint"));
        }
        bitmaps.push((allocation, bitmap));
    }
    Ok(Checkpoint {
        generation: get(&page, 16),
        root,
        bitmaps,
    })
}
