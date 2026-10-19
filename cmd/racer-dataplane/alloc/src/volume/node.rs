//! Tree node codec. See the "Nodes" section of `designs/racer-extent-volume.md`.

use super::format::{KEY_BYTES, Key, NodeAddr, UNIT_BYTES, VALUE_BYTES, Value};

/// Magic at the start of every node.
pub const NODE_MAGIC: [u8; 8] = *b"RCRVNODE";

/// Node format version.
pub const NODE_VERSION: u16 = 1;

/// Bytes in a node header.
pub const NODE_HEADER_BYTES: usize = 64;

/// Most entries in a leaf.
pub const LEAF_CAPACITY: usize = 72;

/// Most children in an inner node.
pub const INNER_CAPACITY: usize = 100;

const LEAF_ENTRY_BYTES: usize = KEY_BYTES + VALUE_BYTES;
const INNER_ENTRY_BYTES: usize = KEY_BYTES + 16;
const KIND_LEAF: u8 = 0;
const KIND_INNER: u8 = 1;
const CHECKSUM_AT: usize = 48;

const _: () = assert!(NODE_HEADER_BYTES + LEAF_CAPACITY * LEAF_ENTRY_BYTES <= UNIT_BYTES);
const _: () = assert!(NODE_HEADER_BYTES + INNER_CAPACITY * INNER_ENTRY_BYTES <= UNIT_BYTES);

/// Contents of a decoded node.
#[derive(Clone, Debug, Eq, PartialEq)]
pub enum NodeBody {
    /// Leaf entries, sorted by key.
    Leaf(Vec<(Key, Value)>),

    /// Inner entries: lowest key of the child and the child's two addresses.
    /// The second address may be `NONE`.
    Inner(Vec<(Key, [NodeAddr; 2])>),
}

/// A node that passed every check in [`decode`].
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct DecodedNode {
    /// Commit that wrote the node.
    pub seq: u64,

    /// Level, 0 for leaves.
    pub level: u8,

    /// Entries.
    pub body: NodeBody,
}

/// What a node read from `addr` must look like.
#[derive(Clone, Copy, Debug)]
pub struct Expect {
    /// Volume id.
    pub volume_id: [u8; 16],

    /// Slots in the volume, to check child addresses.
    pub slot_count: u64,

    /// Address the node was read from.
    pub addr: NodeAddr,

    /// Seq of the superblock. The node may not be newer.
    pub max_seq: u64,

    /// Required level, if known.
    pub level: Option<u8>,

    /// Inclusive lower bound of the node's keys.
    pub low: Key,

    /// Exclusive upper bound of the node's keys, `None` for unbounded.
    pub high: Option<Key>,
}

fn header(
    buf: &mut [u8; UNIT_BYTES],
    volume_id: &[u8; 16],
    seq: u64,
    addr: NodeAddr,
    kind: u8,
    level: u8,
    count: usize,
) {
    buf[0..8].copy_from_slice(&NODE_MAGIC);
    buf[8..10].copy_from_slice(&NODE_VERSION.to_le_bytes());
    buf[10] = kind;
    buf[11] = level;
    buf[12..14].copy_from_slice(&(count as u16).to_le_bytes());
    buf[16..32].copy_from_slice(volume_id);
    buf[32..40].copy_from_slice(&seq.to_le_bytes());
    buf[40..48].copy_from_slice(&addr.0.to_le_bytes());
}

fn seal(buf: &mut [u8; UNIT_BYTES]) {
    let sum = super::format::checksum(&buf[..]);
    buf[CHECKSUM_AT..CHECKSUM_AT + 8].copy_from_slice(&sum.to_le_bytes());
}

/// Encode a leaf. Panics if `keys` and `values` differ in length, are empty, or
/// exceed [`LEAF_CAPACITY`].
pub fn encode_leaf(
    volume_id: &[u8; 16],
    seq: u64,
    addr: NodeAddr,
    keys: &[Key],
    values: &[Value],
) -> Box<[u8; UNIT_BYTES]> {
    assert!(keys.len() == values.len() && !keys.is_empty() && keys.len() <= LEAF_CAPACITY);
    let mut buf = Box::new([0u8; UNIT_BYTES]);
    header(&mut buf, volume_id, seq, addr, KIND_LEAF, 0, keys.len());
    for (i, (key, value)) in keys.iter().zip(values).enumerate() {
        let at = NODE_HEADER_BYTES + i * LEAF_ENTRY_BYTES;
        buf[at..at + KEY_BYTES].copy_from_slice(&key.0);
        buf[at + KEY_BYTES..at + LEAF_ENTRY_BYTES].copy_from_slice(&value.encode());
    }
    seal(&mut buf);
    buf
}

/// Encode an inner node. Panics if `keys` and `children` differ in length, are
/// empty, exceed [`INNER_CAPACITY`], or `level` is 0.
pub fn encode_inner(
    volume_id: &[u8; 16],
    seq: u64,
    addr: NodeAddr,
    level: u8,
    keys: &[Key],
    children: &[[NodeAddr; 2]],
) -> Box<[u8; UNIT_BYTES]> {
    assert!(keys.len() == children.len() && !keys.is_empty() && keys.len() <= INNER_CAPACITY);
    assert!(level > 0);
    let mut buf = Box::new([0u8; UNIT_BYTES]);
    header(
        &mut buf,
        volume_id,
        seq,
        addr,
        KIND_INNER,
        level,
        keys.len(),
    );
    for (i, (key, child)) in keys.iter().zip(children).enumerate() {
        let at = NODE_HEADER_BYTES + i * INNER_ENTRY_BYTES;
        buf[at..at + KEY_BYTES].copy_from_slice(&key.0);
        buf[at + KEY_BYTES..at + KEY_BYTES + 8].copy_from_slice(&child[0].0.to_le_bytes());
        buf[at + KEY_BYTES + 8..at + INNER_ENTRY_BYTES].copy_from_slice(&child[1].0.to_le_bytes());
    }
    seal(&mut buf);
    buf
}

fn u16_at(bytes: &[u8], at: usize) -> u16 {
    u16::from_le_bytes([bytes[at], bytes[at + 1]])
}

fn u64_at(bytes: &[u8], at: usize) -> u64 {
    u64::from_le_bytes(bytes[at..at + 8].try_into().expect("8 bytes"))
}

fn key_at(bytes: &[u8], at: usize) -> Key {
    Key(bytes[at..at + KEY_BYTES].try_into().expect("key bytes"))
}

/// Decode and validate a node. Returns `None` if any check fails: magic,
/// version, checksum, volume id, own address, seq, kind and level, count,
/// key order and range, values, child addresses, and zeroed unused bytes.
pub fn decode(bytes: &[u8; UNIT_BYTES], expect: &Expect) -> Option<DecodedNode> {
    if bytes[0..8] != NODE_MAGIC || u16_at(bytes, 8) != NODE_VERSION {
        return None;
    }
    let mut digest = crc64fast::Digest::new();
    digest.write(&bytes[..CHECKSUM_AT]);
    digest.write(&[0; 8]);
    digest.write(&bytes[CHECKSUM_AT + 8..]);
    if digest.sum64() != u64_at(bytes, CHECKSUM_AT) {
        return None;
    }
    let (kind, level, count) = (bytes[10], bytes[11], usize::from(u16_at(bytes, 12)));
    let seq = u64_at(bytes, 32);
    if u16_at(bytes, 14) != 0
        || u64_at(bytes, 56) != 0
        || bytes[16..32] != expect.volume_id
        || u64_at(bytes, 40) != expect.addr.0
        || seq > expect.max_seq
        || expect.level.is_some_and(|want| want != level)
    {
        return None;
    }
    let (capacity, entry_bytes) = match (kind, level) {
        (KIND_LEAF, 0) => (LEAF_CAPACITY, LEAF_ENTRY_BYTES),
        (KIND_INNER, 1..) => (INNER_CAPACITY, INNER_ENTRY_BYTES),
        _ => return None,
    };
    if count == 0 || count > capacity {
        return None;
    }
    let end = NODE_HEADER_BYTES + count * entry_bytes;
    if bytes[end..].iter().any(|&b| b != 0) {
        return None;
    }
    let mut previous: Option<Key> = None;
    let mut check_key = |key: Key| {
        let ok = match previous {
            Some(prev) => prev < key,
            None => key >= expect.low,
        } && expect.high.is_none_or(|high| key < high);
        previous = Some(key);
        ok
    };
    let body = if kind == KIND_LEAF {
        let mut entries = Vec::with_capacity(count);
        for i in 0..count {
            let at = NODE_HEADER_BYTES + i * entry_bytes;
            let key = key_at(bytes, at);
            let value_bytes: &[u8; VALUE_BYTES] = bytes[at + KEY_BYTES..at + entry_bytes]
                .try_into()
                .expect("value bytes");
            if !check_key(key) {
                return None;
            }
            entries.push((key, Value::decode(value_bytes)?));
        }
        NodeBody::Leaf(entries)
    } else {
        let mut entries = Vec::with_capacity(count);
        for i in 0..count {
            let at = NODE_HEADER_BYTES + i * entry_bytes;
            let key = key_at(bytes, at);
            let first = NodeAddr(u64_at(bytes, at + KEY_BYTES));
            let second = NodeAddr(u64_at(bytes, at + KEY_BYTES + 8));
            if !check_key(key)
                || !first.valid_for(expect.slot_count)
                || !(second.is_none() || second.valid_for(expect.slot_count))
            {
                return None;
            }
            entries.push((key, [first, second]));
        }
        NodeBody::Inner(entries)
    };
    Some(DecodedNode { seq, level, body })
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::volume::format::{RETAIN_FOREVER, Tier};

    const VOLUME: [u8; 16] = [7; 16];

    fn key(n: u32) -> Key {
        Key::new([1; 8], [2; 12], n)
    }

    fn value(n: u64) -> Value {
        Value {
            slot: 3,
            unit: 0,
            class: 0,
            tier: Tier::Storage,
            length: 100,
            flags: 0,
            generation: n + 1,
            retain_until: RETAIN_FOREVER,
        }
    }

    fn expect(addr: NodeAddr, level: Option<u8>) -> Expect {
        Expect {
            volume_id: VOLUME,
            slot_count: 8,
            addr,
            max_seq: 10,
            level,
            low: key(0),
            high: Some(key(1000)),
        }
    }

    fn leaf() -> (NodeAddr, Box<[u8; UNIT_BYTES]>, Vec<(Key, Value)>) {
        let entries: Vec<_> = (0..LEAF_CAPACITY as u32)
            .map(|i| (key(i * 3 + 1), value(u64::from(i))))
            .collect();
        let keys: Vec<_> = entries.iter().map(|e| e.0).collect();
        let values: Vec<_> = entries.iter().map(|e| e.1).collect();
        let addr = NodeAddr::new(2, 9);
        (addr, encode_leaf(&VOLUME, 5, addr, &keys, &values), entries)
    }

    type Encoded<T> = (NodeAddr, Box<[u8; UNIT_BYTES]>, Vec<(Key, T)>);

    fn inner() -> Encoded<[NodeAddr; 2]> {
        let entries: Vec<_> = (0..INNER_CAPACITY as u32)
            .map(|i| {
                let second = if i % 2 == 0 {
                    NodeAddr::NONE
                } else {
                    NodeAddr::new(4, i as u16)
                };
                (key(i * 5), [NodeAddr::new(1, i as u16), second])
            })
            .collect();
        let keys: Vec<_> = entries.iter().map(|e| e.0).collect();
        let children: Vec<_> = entries.iter().map(|e| e.1).collect();
        let addr = NodeAddr::new(0, 2);
        (
            addr,
            encode_inner(&VOLUME, 5, addr, 2, &keys, &children),
            entries,
        )
    }

    #[test]
    fn round_trips() {
        let (addr, bytes, entries) = leaf();
        let node = decode(&bytes, &expect(addr, Some(0))).unwrap();
        assert_eq!((node.seq, node.level), (5, 0));
        assert_eq!(node.body, NodeBody::Leaf(entries));

        let (addr, bytes, entries) = inner();
        let node = decode(&bytes, &expect(addr, None)).unwrap();
        assert_eq!((node.seq, node.level), (5, 2));
        assert_eq!(node.body, NodeBody::Inner(entries));
    }

    #[test]
    fn every_bit_flip_is_rejected() {
        for (addr, bytes) in [(leaf().0, leaf().1), (inner().0, inner().1)] {
            let expect = expect(addr, None);
            assert!(decode(&bytes, &expect).is_some());
            let mut copy = bytes.clone();
            for bit in 0..UNIT_BYTES * 8 {
                copy[bit / 8] ^= 1 << (bit % 8);
                assert!(decode(&copy, &expect).is_none(), "bit {bit}");
                copy[bit / 8] ^= 1 << (bit % 8);
            }
        }
    }

    #[test]
    fn rejects_wrong_context() {
        let (addr, bytes, _) = leaf();
        let good = expect(addr, Some(0));
        let cases: [fn(&mut Expect); 7] = [
            |e| e.addr = NodeAddr::new(2, 10),
            |e| e.volume_id[3] ^= 1,
            |e| e.max_seq = 4,
            |e| e.level = Some(1),
            |e| e.low = key(2),
            |e| e.high = Some(key(3 * 71 + 1)),
            |e| e.slot_count = 2,
        ];
        for (i, mutate) in cases.into_iter().enumerate() {
            let mut bad = good;
            mutate(&mut bad);
            // slot_count only matters for inner nodes.
            if i == 6 {
                let (addr, bytes, _) = inner();
                let mut bad = expect(addr, None);
                mutate(&mut bad);
                assert!(decode(&bytes, &bad).is_none());
                continue;
            }
            assert!(decode(&bytes, &bad).is_none(), "case {i}");
        }
        let mut exact = good;
        exact.low = key(1);
        exact.high = Some(key(3 * 71 + 2));
        exact.max_seq = 5;
        assert!(decode(&bytes, &exact).is_some());
    }

    fn resealed(
        mut bytes: Box<[u8; UNIT_BYTES]>,
        edit: impl FnOnce(&mut [u8; UNIT_BYTES]),
    ) -> Box<[u8; UNIT_BYTES]> {
        edit(&mut bytes);
        bytes[CHECKSUM_AT..CHECKSUM_AT + 8].fill(0);
        seal(&mut bytes);
        bytes
    }

    #[test]
    fn rejects_bad_structure_with_valid_checksum() {
        let (addr, bytes, _) = leaf();
        let e = expect(addr, None);
        let cases: [fn(&mut [u8; UNIT_BYTES]); 7] = [
            |b| b[10] = 1, // inner kind at level 0
            |b| b[11] = 1, // leaf at level 1
            |b| b[12..14].copy_from_slice(&0u16.to_le_bytes()),
            |b| b[12..14].copy_from_slice(&73u16.to_le_bytes()),
            |b| b[NODE_HEADER_BYTES + KEY_BYTES + 7] = 9, // bad tier
            |b| b[14] = 1,
            |b| b[9] = 1,
        ];
        for (i, edit) in cases.into_iter().enumerate() {
            let bad = resealed(bytes.clone(), edit);
            assert!(decode(&bad, &e).is_none(), "case {i}");
        }
        // Order: make entry 1 equal to entry 0.
        let bad = resealed(bytes.clone(), |b| {
            let first: [u8; KEY_BYTES] = b[NODE_HEADER_BYTES..NODE_HEADER_BYTES + KEY_BYTES]
                .try_into()
                .unwrap();
            let at = NODE_HEADER_BYTES + LEAF_ENTRY_BYTES;
            b[at..at + KEY_BYTES].copy_from_slice(&first);
        });
        assert!(decode(&bad, &e).is_none());
        // Nonzero byte past the entries.
        let (addr, bytes, _) = inner();
        let e = expect(addr, None);
        let bad = resealed(bytes.clone(), |b| b[UNIT_BYTES - 1] = 1);
        assert!(decode(&bad, &e).is_none());
        // First child address NONE.
        let bad = resealed(bytes, |b| {
            let at = NODE_HEADER_BYTES + KEY_BYTES;
            b[at..at + 8].copy_from_slice(&u64::MAX.to_le_bytes());
        });
        assert!(decode(&bad, &e).is_none());
    }
}
